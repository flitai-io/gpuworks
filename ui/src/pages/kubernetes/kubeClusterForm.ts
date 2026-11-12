import { z } from "zod";
import type { KubeCluster, KubeClusterRequest, KubeClusterUpdate, KubeDefaults, ProviderClusterPool } from "../../api";

/**
 * A cluster entry made from a cloud account's cluster: what the form starts
 * with, and the source it records. The entry holds node pools of that cluster,
 * so no node selector is asked.
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
export function formSchema(t: (key: string) => string) {
  return z.object({
    name: z.string().refine((value) => value.trim() !== "", t("drawer.nameRequired")),
    server_url: z.string()
      .refine((value) => value.trim() !== "", t("drawer.serverUrlRequired"))
      .refine((value) => value.trim() === "" || URL_PATTERN.test(value), t("drawer.serverUrlInvalid")),
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
export type FormValues = z.infer<ReturnType<typeof formSchema>>;
export type FormField_ = keyof FormValues;

// The request fields a refused cluster can name (KUBE_CLUSTER_INVALID's field)
// and the form field each one belongs to.
export const FIELD_OF: Record<string, FormField_> = {
  name: "name",
  server_url: "server_url",
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
export const ADVANCED_FIELDS: ReadonlySet<FormField_> = new Set<FormField_>([
  "namespace", "agent_image_source", "agent_image", "image_pull_secret", "state_dir", "runtime_class", "containerd_socket", "api_server", "engine_ports", "keep_cards", "idle_return_seconds", "priority", "allow_preemption",
]);

/**
 * A new entry's form starts empty: the server's defaults (GET /api/v1/kube/defaults) are
 * shown as hints and a field left empty is not sent, so the server applies
 * them. Only a cloud prefill brings values of its own. Editing starts from the entry.
 */
export function defaultsOf(prefill?: KubeClusterPrefill, edit?: KubeCluster): FormValues {
  if (edit) {
    return {
      name: edit.name,
      server_url: edit.server_url,
      node_selector: edit.node_selector,
      namespace: edit.namespace,
      state_dir: edit.state_dir,
      agent_image: edit.agent_image,
      agent_image_source: edit.agent_image_source,
      image_pull_secret: edit.image_pull_secret,
      runtime_class: edit.runtime_class,
      containerd_socket: edit.containerd_socket,
      api_server: edit.api_server,
      engine_ports: edit.engine_ports,
      keep_cards: edit.idle_return_seconds < 0,
      idle_return_seconds: edit.idle_return_seconds < 0 ? null : edit.idle_return_seconds,
      priority: edit.priority,
      allow_preemption: edit.allow_preemption,
    };
  }
  return {
    name: prefill?.name ?? "",
    server_url: window.location.origin,
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

export function requestOf(values: FormValues, defaults: KubeDefaults | null, poolIds: string[], prefill?: KubeClusterPrefill): KubeClusterRequest {
  const request: KubeClusterRequest = {
    name: values.name.trim(),
    server_url: values.server_url.trim(),
  };
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
  // Every empty field takes the server's default; an empty RuntimeClass is decided on each node.
  const optional = { runtime_class: values.runtime_class, image_pull_secret: values.image_pull_secret, namespace: values.namespace, state_dir: values.state_dir, agent_image: values.agent_image, agent_image_source: values.agent_image_source, node_selector: prefill ? "" : values.node_selector, containerd_socket: values.containerd_socket, api_server: values.api_server, engine_ports: values.engine_ports } as const;
  for (const [key, value] of Object.entries(optional)) {
    const text = (value ?? "").trim();
    if (text) (request as unknown as Record<string, string>)[key] = text;
  }
  return request;
}

/** The text fields an edit may change; a cloud entry's node selector is not among them (its nodes are chosen by node pool). */
const EDITABLE_TEXT = ["agent_image", "agent_image_source", "image_pull_secret", "runtime_class", "containerd_socket", "api_server", "engine_ports"] as const;

/**
 * What an edit sends: only the fields that differ from the entry (an emptied one is sent
 * empty, which clears it); the namespace, server URL and state directory never are.
 * An empty number takes the server's default, like in a new entry's form.
 */
export function updateOf(values: FormValues, cluster: KubeCluster, defaults: KubeDefaults | null): KubeClusterUpdate {
  const update: KubeClusterUpdate = {};
  const name = values.name.trim();
  if (name !== cluster.name) update.name = name;
  if (!cluster.source && values.node_selector.trim() !== cluster.node_selector) update.node_selector = values.node_selector.trim();
  for (const key of EDITABLE_TEXT) {
    const text = values[key].trim();
    if (text !== cluster[key]) update[key] = text;
  }
  const idle = values.keep_cards ? defaults?.idle_return_keep : values.idle_return_seconds ?? defaults?.idle_return_seconds;
  if (idle !== undefined && idle !== cluster.idle_return_seconds) update.idle_return_seconds = idle;
  const priority = values.priority ?? defaults?.priority;
  if (priority !== undefined && priority !== cluster.priority) update.priority = priority;
  if (values.allow_preemption !== null && values.allow_preemption !== cluster.allow_preemption) update.allow_preemption = values.allow_preemption;
  return update;
}

/** The registry the form is for: the chosen one, else the one the server suggests for the cloud cluster, else its default. */
export function formImageSource(source: string, defaults: KubeDefaults | null, prefill?: KubeClusterPrefill): string {
  return source || prefill?.agent_image_source || defaults?.agent_image_source || "";
}
