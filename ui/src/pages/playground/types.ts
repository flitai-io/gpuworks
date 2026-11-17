import type { LocalizedText } from "../../i18n/localizedText";
import type { InferenceAPIOperation } from "../../inferenceApi";
export type { AsyncLifecycle, ExampleRequest, InferenceAPIOperation as PlaygroundOperation, OperationExample, OperationType } from "../../inferenceApi";

export type Capability = "chat" | "transcription" | "embedding" | "rerank";
export interface PlaygroundModel {
  id: string;
  label: string;
  capability: Capability;
  operations: InferenceAPIOperation[];
  modalities?: string[];
  input_modalities?: string[];
  context_length?: number;
  sleeping: boolean;
  /** Set on a sleeping model whose sleep a request ends: sending wakes it. */
  wakes_on_request?: boolean;
  deployment_id?: number;
  template_id?: number;
  template_spec_source?: string;
  template_spec_name?: string;
  external?: boolean;
  public_data_plane: boolean;
  /** Requests take the priority class the page names; false for an external model. */
  request_priority?: boolean;
  /** The engine orders requests by their class; without it the class is only recorded. */
  priority_scheduling?: boolean;
  /**
   * The extra-parameter buttons for this deployment. Which chat-template field
   * stops thinking depends on the model family and the engine, so the server
   * resolves the values here rather than the page guessing a family.
   */
  extra_parameter_presets?: ExtraParameterPreset[];
}

export interface ParameterDefinition {
  name: string;
  label: LocalizedText;
  kind: "number" | "integer" | "string";
  minimum?: number;
  maximum?: number;
  step?: number;
  default: unknown;
}

// StreamOptionDefinition is one boolean stream_options field the Playground
// adds to a streamed chat request; requires names an option that must be on
// for this one to take effect.
export interface StreamOptionDefinition {
  name: string;
  label: LocalizedText;
  description: LocalizedText;
  default: boolean;
  requires?: string;
}

export interface ExtraParameterPreset {
  key?: string;
  label: LocalizedText;
  /** The patch to merge into Extra Parameters; absent means the preset cannot be honored here. */
  value?: Record<string, unknown>;
  /** The declared capability that makes value effective; empty means value is only an example. */
  applies_when?: string;
  /** Why the preset is unavailable, and what to add by hand instead. Set only when value is absent. */
  unavailable?: LocalizedText;
}

export interface PlaygroundCatalog {
  models: PlaygroundModel[];
  parameters: ParameterDefinition[];
  stream_options?: StreamOptionDefinition[];
  /** The policy HTML previews of replies run under (platform security settings). */
  html_preview: { allow_network: boolean };
}

export interface ParameterState {
  enabled: Record<string, boolean>;
  values: Record<string, unknown>;
  systemPrompt: string;
  extraJSON: string;
  // Absent in sessions saved before stream options were switchable; every
  // option then takes its default.
  streamOptions?: Record<string, boolean>;
  // The request priority class to ask for; absent or empty asks for none,
  // and the request runs at the default class.
  priorityClass?: string;
}

export interface Attachment {
  id: string;
  name: string;
  type: "image" | "audio";
  dataURL: string;
}

export interface Usage {
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
  prompt_tokens_details?: { cached_tokens?: number };
  [key: string]: unknown;
}

export interface PerformanceMetrics {
  ttftMS?: number;
  tpotMS?: number;
  tps?: number;
  prefillTPS?: number;
  durationMS: number;
  outputTokens?: number;
  // Token counts were estimated from chunks: the endpoint reported no usage.
  approximate?: boolean;
}

export interface ChatMessage {
  id: string;
  role: "user" | "assistant";
  content: string;
  reasoning?: string;
  attachments?: Attachment[];
  usage?: Usage;
  metrics?: PerformanceMetrics;
  error?: string;
  streaming?: boolean;
  // The model that answered (assistant messages).
  model?: string;
  // The request priority class the gateway says the reply ran at.
  priorityClass?: string;
  // The page instance generating a streaming reply; a reply left streaming by
  // a page that is gone was interrupted.
  owner?: string;
  // Generation was cut off by a reload or a closed tab.
  interrupted?: boolean;
}

export interface CompletionMessage {
  role: "system" | "user" | "assistant";
  content: unknown;
}
