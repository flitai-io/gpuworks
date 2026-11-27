import type { ProductMessage } from "../apiError";

// One selectable acquisition option. `key` is the opaque token the server
// validates on submit; the client never composes one itself.
export interface AcquisitionCandidate {
  key: string;
  label: string;
  // Present when `label` is an identifier: show this to the user and keep
  // submitting `label`.
  display_name?: string;
  reason: string;
  recommended?: boolean;
  // Storage fields appear on present and target candidates.
  storage_id?: number;
  storage_type?: string;
  size_bytes?: number;
  /** nfs_cache: bytes still to copy into the node cache before a start. */
  cache_bytes?: number;
  /** The files are already on the target node's disk. */
  on_node?: boolean;
  /** A reason code: the storage is listed but cannot serve the target nodes now. */
  unavailable?: string;
  default?: boolean;
}

export interface AcquisitionResult {
  /** One candidate per storage that holds the model and the target nodes can read. */
  present?: AcquisitionCandidate[];
  /** Writable storages a node may save the model into when it has to fetch it. */
  targets?: AcquisitionCandidate[];
  /** The template's public weight sources: which weights run. */
  providers?: AcquisitionCandidate[];
  image?: {
    image?: string;
    cached?: boolean;
    cache_reason?: string;
    options?: ImageOption[];
  };
  /**
   * A forecast per node the request names (a manual placement or a
   * multi-node draft, in rank order); absent for automatic placement.
   */
  nodes?: AcquisitionNodeFact[];
  /**
   * How each template component other than the primary (a separate text
   * encoder, a speculative draft) reaches the target nodes; present once the
   * nodes are known.
   */
  components?: AcquisitionComponent[];
}

/**
 * One template component's forecast. plan "ready": every target node reads it
 * already; "shared_download": the create downloads it once into the shared
 * storage `storage`, through one writer node (follows_primary: no storage was
 * chosen and it is the one the primary sits on); "node_download": each target
 * node without it downloads it into its own storage.
 */
export interface AcquisitionComponent {
  name: string;
  role: string;
  model: string;
  size_bytes?: number;
  plan: "ready" | "shared_download" | "node_download";
  nodes: number;
  ready_nodes: number;
  storage_id?: number;
  storage?: string;
  follows_primary?: boolean;
}

export type ImagePlan = "present" | "copy" | "pull";
export type WeightsPlan = "present" | "copy" | "storage" | "download";

/** How one node would get the image and the weights if it started now. */
export interface AcquisitionNodeFact {
  node_uuid: string;
  node_name: string;
  image_cached?: boolean;
  image_cache_reason?: string;
  image_plan: ImagePlan | string;
  image_copy_from?: string;
  image_pull_refs?: string[];
  /** The node reorders the image's own copies by how fast it reaches them. */
  image_pull_ranked?: boolean;
  /** The first image_pull_fixed refs are rule endpoints, tried in this order before the rest. */
  image_pull_fixed?: number;
  /** Why the node has nothing to pull from: a product error code, its params and the English fallback. */
  image_pull_error?: { code: string; params?: Record<string, unknown>; message: string };
  weights_present: boolean;
  weights_plan: WeightsPlan | string;
  weights_copy_from?: string;
  weights_storage?: string;
  weights_provider?: string;
  /** The hosts a download of the weights would try, in order (planned from the site rules). */
  weights_fetch_hosts?: string[];
  /** Why the node cannot download the weights (WEIGHTS_FETCH_FORBIDDEN: the rules forbid every address). */
  weights_fetch_error?: ProductMessage;
}

export interface ImageOption {
  image: string;
  reason: string;
  compatible: boolean;
  /** What the image catalog says the image runs; absent outside the catalog. */
  engine?: string;
  engine_version?: string;
  /** The engine choosing this image selects, when not the draft's own. */
  selects_engine?: string;
}

/** A node selector: for each label key the values a node may carry (alternatives); keys all hold. Empty = every node. */
export type NodeRequirementsMap = Record<string, string[]>;
