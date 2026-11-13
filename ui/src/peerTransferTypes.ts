export interface PeerTransferObservation {
  transfer_id: string;
  kind: string;
  operation: string;
  source_node_uuid: string;
  target_node_uuid: string;
  resource_key?: string;
  transport?: string;
  phase: string;
  reason?: string;
  fallback_reason?: string;
  started_at?: string;
  updated_at?: string;
  last_progress_at?: string;
  source_to_target_bytes: number;
  target_to_source_bytes: number;
  effective_rate_limit_bps: number;
  server_rate_bps: number;
  rate_window_seconds: number;
  rate_wait_ms: number;
}

export interface PeerTransferSnapshot {
  observed_at: string;
  server_started_at?: string;
  scope: "server_process";
  policy: {
    rate_limit_bps: number;
    burst_bytes: number;
    max_active: number;
    max_per_node: number;
    no_progress_timeout_seconds: number;
    confirmation_timeout_seconds: number;
  };
  active_relay_count: number;
  rate_waiting_count: number;
  totals: {
    started: number;
    peer_confirmed: number;
    failed: number;
    canceled: number;
    busy: number;
    probe_started: number;
    probe_ok: number;
    probe_failed: number;
    source_to_target_bytes: number;
    target_to_source_bytes: number;
    rate_wait_ms: number;
  };
  active: PeerTransferObservation[];
}
