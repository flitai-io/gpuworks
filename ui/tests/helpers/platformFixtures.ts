// Fixtures for the external platforms list (ui/src/pages/CloudProvidersPage.tsx)
// and what it is built from, shaped as the server answers: an account carries
// the category, resources and node count the server derives; a kind carries
// its platform type and the resources it connects; a cluster entry carries
// its pools and what the server allows on it; an instance carries the
// actions the server allows on it, and a cloud's cluster its GPU node pools.
import type { CloudProvider, KubeCluster, KubeDefaults, List, ProviderCapabilities, ProviderCluster, ProviderClusterPool, ProviderInstance, ProviderKindInfo } from "../../src/api";
import { list } from "./listPage.ts";

export const capabilities: ProviderCapabilities = {
  can_list_instances: true, can_power_control: true, can_install_agent: true, can_create: false,
  can_delete: false, can_inject_user_data: false, has_pricing: false,
};

// GET /api/v1/providers/kinds: category follows provider.CategoryOf, resources provider.ResourcesOf.
export const aliyunKind: ProviderKindInfo = {
  kind: "aliyun", label: { en: "Alibaba Cloud", zh: "阿里云" }, category: "public_cloud", homepage: "https://www.aliyun.com", capabilities, credential_fields: [],
  server_dial_supported: false, can_list_clusters: true, resources: [{ type: "vm", product: "ECS" }, { type: "kubernetes", product: "ACK" }],
};
export const runpodKind: ProviderKindInfo = {
  kind: "runpod", label: { en: "RunPod", zh: "RunPod" }, category: "public_cloud", homepage: "https://www.runpod.io", capabilities, credential_fields: [],
  server_dial_supported: false, resources: [{ type: "container", product: "Pod" }],
};
export const zstackKind: ProviderKindInfo = {
  kind: "zstack", label: { en: "ZStack Cloud", zh: "ZStack Cloud" }, category: "private_cloud", homepage: "https://www.zstack.io", capabilities, credential_fields: [],
  server_dial_supported: true, resources: [{ type: "host" }],
};
export const KINDS = [aliyunKind, runpodKind, zstackKind];

/**
 * An account as GET /api/v1/providers lists it: the resources are its kind's,
 * in the kind's order, and a kind the fixture does not know connects nothing,
 * as provider.ResourcesOf answers. instance_count is how many instances the
 * last sync found, synced_at when it ran; node_count the compute nodes that
 * joined through it.
 */
export function account(id: number, name: string, kind: string, overrides: Partial<CloudProvider> = {}): CloudProvider {
  const info = KINDS.find((each) => each.kind === kind);
  return {
    id, name, kind, category: info?.category || "public_cloud", state: "valid", instance_count: 0,
    resources: info?.resources ?? [], node_count: 0, synced_at: "2026-11-10T07:55:00Z", created_at: "2026-11-01T08:00:00Z", updated_at: "2026-11-10T08:00:00Z", ...overrides,
  };
}

export const APPLY_COMMAND = "kubectl apply -f 'https://gpuworks.example/kube/manifest.yaml?token=secret-token'";

/**
 * A cluster entry as GET /api/v1/kube/clusters lists it to an administrator:
 * made by hand unless a source is given. Its joined nodes are counted by the
 * vendor of their cards (each agent reports its own node's), most first.
 */
export function kubeEntry(overrides: Partial<KubeCluster> = {}): KubeCluster {
  return {
    id: 1, uuid: "c-1", name: "prod-gpu", namespace: "gpuworks-system", server_url: "https://gpuworks.example",
    state_dir: "/opt/gpuworks", agent_image: "", agent_image_source: "dockerhub", effective_agent_image: "gpuworks/gpuworks-agent:v1",
    node_selector: "nvidia.com/gpu.present=true", runtime_class: "", image_pull_secret: "",
    containerd_socket: "", api_server: "", engine_ports: "40000-42767", idle_return_seconds: 600, priority: 10000, allow_preemption: false,
    cluster_uid: "7f3c2a10-aaaa-bbbb-cccc-0123456789ab", revoked: false, node_count: 3, vendors: [{ vendor: "nvidia", nodes: 3 }],
    created_at: "2026-10-19T08:30:00Z", apply_command: APPLY_COMMAND, stalled: false, pool_ids: [], namespace_shared_with: [],
    can_apply: false, can_remove: false, operation_blocked: "no_source", ...overrides,
  };
}

/** An entry made from a cluster of an account: it holds node pools and the server may apply to it. */
export function cloudEntry(overrides: Partial<KubeCluster> = {}): KubeCluster {
  return kubeEntry({
    id: 5, uuid: "e-5", name: "ack-gpu", node_selector: "", cluster_uid: "", node_count: 0, vendors: [], pool_ids: ["np-l20"],
    can_apply: true, can_remove: true, operation_blocked: undefined,
    source: { provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun", cluster_id: "c-gpu" }, ...overrides,
  });
}

/**
 * An instance as GET /api/v1/providers/{id}/instances lists it: a running VM
 * no agent is installed on, reached over one direct SSH connection. The
 * server leaves out the nodes of the account's managed clusters, and says
 * what each instance allows now (provider.InstanceActions).
 */
export function instance(overrides: Partial<ProviderInstance> = {}): ProviderInstance {
  return {
    external_id: "i-uf6train01", name: "train-01", gpu_model: "NVIDIA A10", gpu_count: 1, power: "running", price_per_hr: 0, region: "cn-shanghai",
    public_ip: "203.0.113.5", ssh_port: 22, ssh_user: "root",
    ssh_connections: [{ id: "direct", type: "direct", label: { en: "Direct", zh: "直连" }, host: "203.0.113.5", port: 22, user: "root", install_supported: true }],
    kind: "vm", node_id: 0, actions: ["power_off", "install"], ...overrides,
  };
}

/** A GPU node pool of a cloud's cluster, held by no entry. */
export const l20Pool: ProviderClusterPool = { id: "np-l20", name: "l20", gpu_model: "NVIDIA L20", gpu_count: 1, nodes: 2, vendor: "nvidia", in_entry: false };

/** A managed cluster as GET /api/v1/providers/{id}/clusters lists it, read live: ready, publicly reachable, one GPU pool, no entry made from it. */
export function cloudCluster(id: string, name: string, overrides: Partial<ProviderCluster> = {}): ProviderCluster {
  return {
    external_id: id, name, region: "cn-shanghai", version: "1.35.7-aliyun.1", type: "ack_managed", type_label: { en: "ACK managed", zh: "ACK 托管版" },
    state: "running", running: true, public_api: true, gpu_pools: [l20Pool], agent_image_source: "dockerhub", ...overrides,
  };
}

/** The same cluster once an entry was made from it: the entry is named and holds the given pools (every pool by default). */
export function connectedCluster(id: string, name: string, entryUUID: string, overrides: Partial<ProviderCluster> = {}, heldPools?: string[]): ProviderCluster {
  const cluster = cloudCluster(id, name, overrides);
  return { ...cluster, entry: { uuid: entryUUID, name }, gpu_pools: cluster.gpu_pools.map((pool) => ({ ...pool, in_entry: heldPools ? heldPools.includes(pool.id) : true })) };
}

// GET /api/v1/kube/defaults.
export const KUBE_DEFAULTS: KubeDefaults = {
  namespace: "gpuworks-system", state_dir: "/opt/gpuworks", engine_ports: "40000-42767", idle_return_seconds: 60, idle_return_keep: -1,
  agent_image: "gpuworks/gpuworks-agent:v1", stalled_after_seconds: 300, agent_image_source: "dockerhub",
  agent_image_sources: [{ id: "dockerhub", image: "gpuworks/gpuworks-agent:v1" }, { id: "swr", image: "swr.example/gpuworks-agent:v1" }],
  priority: 10000, priority_max: 1000000000, allow_preemption: false,
};

export interface PlatformServer {
  /** How often each list was read. */
  reads: { providers: number; clusters: number };
  /** What the lists answer; replace a list with an Error to fail its next reads. */
  providers: CloudProvider[] | Error;
  clusters: KubeCluster[] | Error;
}

/**
 * Stubs what the page reads on its own: the session, the two lists and the
 * kind schema. A viewer gets the entries without their connect command, as
 * the server sends them. The returned object is live: change its lists and
 * the next read answers with them. `api` is the client under test (imported
 * by the test after its DOM globals exist); the test restores it afterwards.
 */
export function servePlatforms(api: object, options: { providers?: CloudProvider[] | Error; clusters?: KubeCluster[] | Error; kinds?: ProviderKindInfo[] | Error; admin?: boolean } = {}): PlatformServer {
  const admin = options.admin !== false;
  const server: PlatformServer = { reads: { providers: 0, clusters: 0 }, providers: options.providers ?? [], clusters: options.clusters ?? [] };
  const kinds = options.kinds ?? KINDS;
  Object.assign(api, {
    me: async () => ({ id: 1, username: admin ? "admin" : "viewer", role: admin ? "admin" : "viewer" }),
    providers: async (): Promise<List<CloudProvider>> => {
      server.reads.providers += 1;
      if (server.providers instanceof Error) throw server.providers;
      return list(server.providers);
    },
    kubeClusters: async (): Promise<List<KubeCluster>> => {
      server.reads.clusters += 1;
      if (server.clusters instanceof Error) throw server.clusters;
      return list(server.clusters.map((cluster) => admin ? cluster : { ...cluster, apply_command: undefined }));
    },
    providerKinds: async () => {
      if (kinds instanceof Error) throw kinds;
      return list(kinds);
    },
    // What an opened account reads: no instance synchronized, no managed cluster in the cloud.
    providerInstances: async () => list([]),
    providerClusters: async () => list([]),
    kubeDefaults: async () => KUBE_DEFAULTS,
  });
  return server;
}
