// Wire types of diagnostics (dto/diagnostics.go): the debug session at
// /api/v1/diagnostics/debug-session and the collections at
// /api/v1/diagnostics/bundles.

export interface DebugSessionLimits {
  default_duration_seconds: number;
  min_duration_seconds: number;
  max_duration_seconds: number;
  default_max_bytes: number;
  max_max_bytes: number;
  node_max_bytes: number;
  request_max_bytes: number;
  keep_days: number;
}

export interface DebugSessionNode {
  uuid: string;
  name: string;
  // collecting, skipped (reason offline / unsupported) or failed.
  state: string;
  reason?: string;
}

export interface DebugSession {
  id: string;
  started_by: string;
  started_at: string;
  until: string;
  max_bytes: number;
  deployment_ids: number[];
  nodes: DebugSessionNode[];
  open: boolean;
  // deadline, size_limit or stopped.
  ended?: string;
  ended_at?: string;
  server_log_bytes: number;
  request_bytes: number;
  requests: number;
}

export interface DebugSessionState {
  session: DebugSession | null;
  limits: DebugSessionLimits;
}

export interface DebugSessionStart {
  duration_seconds?: number;
  max_bytes?: number;
  deployment_ids?: number[];
  node_uuids?: string[];
}

/** One part of a collection, as a check: pass (collected whole), warn (cut at
 * its size bound), skip (not read; code says why) or fail. */
export interface DiagnosticsBundleItem {
  id: string;
  path: string;
  status: string;
  code?: string;
  params?: Record<string, unknown>;
  message?: string;
  /** The failed read's own error text. */
  evidence?: string;
  bytes: number;
}

export interface DiagnosticsBundle {
  id: string;
  requested_by: string;
  created_at: string;
  finished_at?: string;
  from: string;
  to: string;
  deployment_ids: number[];
  instance_uuids: string[];
  node_uuids: string[];
  doctor: boolean;
  // running, ready or failed.
  state: string;
  error?: string;
  size_bytes: number;
  items: DiagnosticsBundleItem[];
}

export interface DiagnosticsBundlesLimits {
  default_window_seconds: number;
  max_window_seconds: number;
  max_nodes: number;
  max_instances: number;
  keep_days: number;
}

export interface DiagnosticsBundles {
  bundles: DiagnosticsBundle[];
  limits: DiagnosticsBundlesLimits;
}

export interface DiagnosticsBundleStart {
  from?: string;
  to?: string;
  deployment_ids?: number[];
  instance_uuids?: string[];
  node_uuids?: string[];
  doctor?: boolean;
}

/** diagnosticsBundleDownloadURL is where a ready collection downloads from. */
export function diagnosticsBundleDownloadURL(id: string): string {
  return `/api/v1/diagnostics/bundles/${encodeURIComponent(id)}/download`;
}
