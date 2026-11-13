export type OperationType =
  | "openai.chat"
  | "openai.completions"
  | "openai.embeddings"
  | "openai.rerank"
  | "openai.audio.transcribe"
  | "openai.audio.speech"
  | "openai.image.generate"
  | "openai.image.edit"
  | "openai.video.generate"
  | "openai.realtime";

export interface AsyncLifecycle {
  id_field: string;
  status_field: string;
  success_statuses: string[];
  failure_statuses: string[];
  poll_method: string;
  poll_endpoint: string;
  content_method: string;
  content_endpoint: string;
  cancel_method: string;
  cancel_endpoint: string;
}

export interface ExampleRequest {
  method: string;
  endpoint: string;
  content_type?: string;
  body?: Record<string, unknown>;
  form?: Record<string, string>;
}

export interface OperationExample {
  request: ExampleRequest;
  follow_ups?: ExampleRequest[];
  curl: string;
  python: string;
}

export interface DiscoveryRoute {
  name: string;
  endpoint: string;
}

export interface InferenceAPIOperation {
	request_defaults?: Record<string, unknown>;
	inputs?: InputField[];
  primary_inputs?: string[];
  discovery?: DiscoveryRoute[];
  type: OperationType;
  endpoint: string;
  mode: "sync" | "stream" | "async" | "realtime";
	request_encoding?: "json" | "multipart" | "websocket";
  request_encodings: Array<"json" | "multipart" | "websocket">;
  renderer: "chat" | "completion" | "audio" | "vector" | "rerank" | "image" | "video" | "realtime";
  response: "json" | "sse" | "audio" | "image" | "video" | "websocket";
  async?: AsyncLifecycle;
  example?: OperationExample;
  /** Declared working values of text primary inputs (prompt, input, query)
   * for a model that needs a particular format; the Playground starts with
   * them. */
  example_input?: Record<string, string>;
  /** How many reference images an image edit takes, resolved by the server
   * (at least one for openai.image.edit, absent otherwise). */
  max_reference_images?: number;
}

export interface InputField {
  name: string;
  kind: "text" | "number" | "select" | "json" | "audio" | "image" | "file";
  encoding?: "data_url" | "multipart";
  required?: boolean;
  label?: import("./i18n/localizedText").LocalizedText;
  options?: string[];
  default?: unknown;
}

export interface InferenceAPIContract {
  health_endpoint?: string;
  operations: InferenceAPIOperation[];
  metrics?: InferenceAPIMetrics;
  /** api.request_stats: reply field path → request stat (queue_seconds,
   * prefill_seconds, decode_seconds), or {to, scale}. */
  request_stats?: Record<string, string | { to: string; scale?: number }>;
}

/** A spec's api.metrics: the engine's Prometheus endpoint and its mapping onto
 * platform inference metrics. A map value is a metric name, or {to, scale}. */
export interface InferenceAPIMetrics {
  endpoint?: string;
  // "json": the endpoint answers one JSON document and map keys are dotted
  // paths of numbers in it; absent means Prometheus text.
  format?: "prometheus" | "json";
  inherit?: string;
  map?: Record<string, string | { to: string; scale?: number }>;
}
