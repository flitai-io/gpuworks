import { useEffect, useMemo, useRef, useState } from "react";
import { useForm, useWatch } from "react-hook-form";
import { z } from "zod";
import { zodResolver } from "@hookform/resolvers/zod";
import { useTranslation } from "react-i18next";
import { api, type KubeCluster, type KubeClusterRequest, type KubeDefaults, type KubeVendor, type ProviderClusterPool } from "../../api";
import { APIError, apiErrorText } from "../../apiError";
import { useKubeDefaults } from "../../kubeClusters";
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from "../../components/ui/accordion";
import { Button } from "../../components/ui/button";
import { Input } from "../../components/ui/input";
import { Switch } from "../../components/ui/switch";
import { FormField } from "../../ui-kit/form/FormField";
import { formSubmit } from "../../ui-kit/form/submit";
import { Notice } from "../../ui-kit/Notice";
import { NumberInput } from "../../ui-kit/NumberInput";
import { OptionSelect } from "../../ui-kit/OptionSelect";
import { SideDrawer } from "../../ui-kit/SideDrawer";
import ApplyCommand from "./ApplyCommand";
import KubePoolPicker from "./KubePoolPicker";
import KubeClusterOperationModal from "./KubeClusterOperationModal";

/**
 * A cluster entry made from a cloud account's cluster: what the form starts
 * with, and the source it records. The entry holds node pools of that cluster
 * and takes their vendor, so neither the vendor nor a node selector is asked.
 */
export interface KubeClusterPrefill {
  name: string;
  /** The cluster's GPU node pools, to choose from. */
  pools: ProviderClusterPool[];
  /** The pools chosen when the form opens. */
  pool_ids: string[];
  provider_id: number;
  provider_cluster_id: string;
  /** The registry the server suggests for the cluster's region. */
  agent_image_source?: string;
  /** "Account · cluster" as the drawer names the source. */
  source_label: string;
}

const URL_PATTERN = /^\s*https?:\/\/\S+\s*$/i;

/** The form's fields. Only what the server states as required is checked here; the server judges the rest. */
function formSchema(t: (key: string) => string) {
  return z.object({
    name: z.string().refine((value) => value.trim() !== "", t("drawer.nameRequired")),
    server_url: z.string()
      .refine((value) => value.trim() !== "", t("drawer.serverUrlRequired"))
      .refine((value) => value.trim() === "" || URL_PATTERN.test(value), t("drawer.serverUrlInvalid")),
    vendor: z.string(),
    node_selector: z.string(),
    namespace: z.string(),
    state_dir: z.string(),
    agent_image: z.string(),
    agent_image_source: z.string(),
    image_pull_secret: z.string(),
    runtime_class: z.string(),
    containerd_socket: z.string(),
    api_server: z.string(),
    engine_ports: z.string(),
    keep_cards: z.boolean(),
    idle_return_seconds: z.number().nullable(),
    priority: z.number().nullable(),
    allow_preemption: z.boolean().nullable(),
  });
}
type FormValues = z.infer<ReturnType<typeof formSchema>>;
type FormField_ = keyof FormValues;

// The request fields a refused cluster can name (KUBE_CLUSTER_INVALID's field)
// and the form field each one belongs to.
const FIELD_OF: Record<string, FormField_> = {
  name: "name",
  server_url: "server_url",
  vendor: "vendor",
  state_dir: "state_dir",
  node_selector: "node_selector",
  namespace: "namespace",
  agent_image: "agent_image",
  agent_image_source: "agent_image_source",
  image_pull_secret: "image_pull_secret",
  runtime_class: "runtime_class",
  containerd_socket: "containerd_socket",
  api_server: "api_server",
  engine_ports: "engine_ports",
  idle_return_seconds: "idle_return_seconds",
  priority: "priority",
};

// The fields inside the collapsed "Advanced" section: a refusal naming one opens it.
const ADVANCED_FIELDS: ReadonlySet<FormField_> = new Set<FormField_>([
  "namespace", "agent_image_source", "agent_image", "image_pull_secret", "state_dir", "runtime_class", "containerd_socket", "api_server", "engine_ports", "keep_cards", "idle_return_seconds", "priority", "allow_preemption",
]);

/**
 * The form starts empty: the server's defaults (GET /api/v1/kube/defaults) are
 * shown as hints and a field left empty is not sent, so the server applies
 * them. Only a cloud prefill brings values of its own.
 */
function defaultsOf(prefill?: KubeClusterPrefill): FormValues {
  return {
    name: prefill?.name ?? "",
    server_url: window.location.origin,
    vendor: "",
    node_selector: "",
    namespace: "",
    state_dir: "",
    agent_image: "",
    agent_image_source: "",
    image_pull_secret: "",
    runtime_class: "",
    containerd_socket: "",
    api_server: "",
    engine_ports: "",
    keep_cards: false,
    idle_return_seconds: null,
    priority: null,
    allow_preemption: null,
  };
}

function requestOf(values: FormValues, defaults: KubeDefaults | null, poolIds: string[], prefill?: KubeClusterPrefill): KubeClusterRequest {
  const request: KubeClusterRequest = {
    name: values.name.trim(),
    server_url: values.server_url.trim(),
  };
  // An entry from a cloud cluster has the vendor of its pools, which the server reads from the cloud.
  if (values.vendor && !prefill) request.vendor = values.vendor as KubeVendor;
  // A cleared number takes the server's default, like every other empty field.
  if (values.keep_cards && defaults) request.idle_return_seconds = defaults.idle_return_keep;
  else if (values.idle_return_seconds !== null) request.idle_return_seconds = values.idle_return_seconds;
  // Untouched, both take the server's defaults.
  if (values.priority !== null) request.priority = values.priority;
  if (values.allow_preemption !== null) request.allow_preemption = values.allow_preemption;
  if (prefill) {
    request.provider_id = prefill.provider_id;
    request.provider_cluster_id = prefill.provider_cluster_id;
    request.provider_pool_ids = poolIds;
  }
  // Only a vendor the server says takes a RuntimeClass gets one (the others have their devices through the agent's
  // device plugin). Every empty field takes the server's default; an empty RuntimeClass is decided on each node.
  const optional = { runtime_class: runtimeClassApplies(defaults, formVendor(values.vendor, poolIds, prefill)) ? values.runtime_class : "", image_pull_secret: values.image_pull_secret, namespace: values.namespace, state_dir: values.state_dir, agent_image: values.agent_image, agent_image_source: values.agent_image_source, node_selector: prefill ? "" : values.node_selector, containerd_socket: values.containerd_socket, api_server: values.api_server, engine_ports: values.engine_ports } as const;
  for (const [key, value] of Object.entries(optional)) {
    const text = (value ?? "").trim();
    if (text) (request as unknown as Record<string, string>)[key] = text;
  }
  return request;
}

/** The vendor the form is for: the chosen one, or for a cloud entry the vendor of its first chosen pool; empty means the server's default. */
function formVendor(vendor: string, poolIds: string[], prefill?: KubeClusterPrefill): string {
  return prefill ? (prefill.pools.find((p) => poolIds.includes(p.id))?.vendor ?? "") : vendor;
}

/** The registry the form is for: the chosen one, else the one the server suggests for the cloud cluster, else its default. */
function formImageSource(source: string, defaults: KubeDefaults | null, prefill?: KubeClusterPrefill): string {
  return source || prefill?.agent_image_source || defaults?.agent_image_source || "";
}

/** Whether the vendor (empty: the server's default one) takes a RuntimeClass; true while the dictionary is unknown, the server ignores it where it does not apply. */
function runtimeClassApplies(defaults: KubeDefaults | null, vendor: string): boolean {
  if (!defaults) return true;
  const id = vendor || defaults.vendor;
  return defaults.vendors.find((v) => v.id === id)?.runtime_class ?? false;
}

/**
 * Connects a Kubernetes cluster: the form, then the command to run in the
 * cluster. The list reloads as soon as the cluster exists; the drawer stays
 * on the command until it is closed.
 */
export default function KubeClusterDrawer({ open, onClose, onCreated, prefill }: { open: boolean; onClose: () => void; onCreated: () => void; prefill?: KubeClusterPrefill }) {
  const { t } = useTranslation("kubernetes");
  const { t: tCommon } = useTranslation("common");
  const schema = useMemo(() => formSchema(t), [t]);
  const form = useForm<FormValues>({ resolver: zodResolver(schema), shouldFocusError: false, defaultValues: defaultsOf(prefill) });
  const [keepCards, vendor, imageSource, allowPreemption] = useWatch({ control: form.control, name: ["keep_cards", "vendor", "agent_image_source", "allow_preemption"] });
  const { defaults, error: defaultsError } = useKubeDefaults(open);
  const formRef = useRef<HTMLFormElement>(null);
  const nameInput = useRef<HTMLInputElement | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [created, setCreated] = useState<KubeCluster | null>(null);
  const [poolIds, setPoolIds] = useState<string[]>(prefill?.pool_ids ?? []);
  const [applying, setApplying] = useState(false);
  const [advancedOpen, setAdvancedOpen] = useState(false);

  // Each opening starts from the defaults (and the prefill), like a fresh form.
  useEffect(() => {
    if (!open) return;
    form.reset(defaultsOf(prefill));
    setPoolIds(prefill?.pool_ids ?? []);
    setCreated(null);
    setError(null);
    setApplying(false);
    setAdvancedOpen(false);
    // The drawer takes focus itself when it opens; hand it to the first field once it has.
    const timer = setTimeout(() => nameInput.current?.focus(), 0);
    return () => clearTimeout(timer);
  }, [open, prefill]);

  const submit = async (values: FormValues) => {
    setSaving(true);
    setError(null);
    try {
      const cluster = await api.createKubeCluster(requestOf(values, defaults, poolIds, prefill));
      setCreated(cluster);
      onCreated();
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
      <FormField control={form.control} name="namespace" label={t("drawer.namespace")} description={t("drawer.namespaceHelp")}>
        {(binding) => <Input {...binding} placeholder={defaults?.namespace} />}
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
      <FormField control={form.control} name="state_dir" label={t("drawer.stateDir")} description={t("drawer.stateDirHelp")}>
        {(binding) => <Input {...binding} placeholder={defaults?.state_dir} />}
      </FormField>
      {runtimeClassApplies(defaults, formVendor(vendor, poolIds, prefill)) && (
        <FormField control={form.control} name="runtime_class" label={t("drawer.runtimeClass")} description={t("drawer.runtimeClassHelp")}>
          {(binding) => <Input {...binding} placeholder={t("drawer.runtimeClassPlaceholder")} />}
        </FormField>
      )}
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

  const body = created ? (
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
      <FormField control={form.control} name="server_url" label={t("drawer.serverUrl")} description={t("drawer.serverUrlHelp")} required>
        {(binding) => <Input {...binding} />}
      </FormField>
      {prefill && (
        <div className="flex flex-col gap-1.5">
          <strong className="font-semibold text-fg-default">{t("drawer.pools")}</strong>
          <span className="text-fg-muted">{t("drawer.poolsHelp")}</span>
          <KubePoolPicker pools={prefill.pools} selected={poolIds} onChange={setPoolIds} disabled={saving} label={t("drawer.pools")} />
        </div>
      )}
      {!prefill && <FormField control={form.control} name="vendor" label={t("drawer.vendor")} description={t("drawer.vendorHelp")}>
        {({ ref: _ref, onChange, ...binding }) => (
          <OptionSelect {...binding} value={binding.value || defaults?.vendor} onChange={onChange} options={(defaults?.vendors ?? []).map(({ id }) => ({ value: id, label: tCommon(`vendor.${id}`) }))} />
        )}
      </FormField>}
      {!prefill && <FormField control={form.control} name="node_selector" label={t("drawer.nodeSelector")} description={t("drawer.nodeSelectorHelp")}>
        {(binding) => <Input {...binding} placeholder={t("drawer.nodeSelectorPlaceholder")} />}
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
      title={t("drawer.title")}
      width={560}
      busy={saving}
      footer={created ? (
        <Button variant="primary" onClick={onClose}>{t("drawer.close")}</Button>
      ) : (
        <>
          <Button variant="ghost" onClick={onClose} disabled={saving}>{t("drawer.cancel")}</Button>
          <Button type="submit" form="kube-cluster-form" variant="primary" loading={saving}>{t("drawer.submit")}</Button>
        </>
      )}
    >
      {body}
    </SideDrawer>
  );
}
