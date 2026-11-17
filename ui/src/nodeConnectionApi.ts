import { sendJSON, type NodeDoctorReport } from "./api";

export interface NodeConnectionCheck {
  stage: "agent_control" | "ssh" | "configuration" | "node_http";
  outcome: string;
  vantage: "control_plane" | "node";
  target?: string;
  source?: "state_file" | "legacy_arguments";
  observed_at?: string;
  http_status?: number;
}

export interface NodeConnectionDiagnostics {
  node_uuid: string;
  checked_at: string;
  connect_mode: string;
  saved_server_url: string;
  last_seen_at: string | null;
  recovery_phase: string;
  recovery_attempts: number;
  last_probe_at: string | null;
  last_recovery_error: string;
  last_recovery_code?: string;
  last_reject_reason?: string;
  last_reject_at?: string;
  agent_report?: NodeDoctorReport;
  exposure: { url: string; state: string; observed_at: string } | null;
  checks: NodeConnectionCheck[];
  guidance: string[];
}

export const nodeConnectionApi = {
  diagnose: (uuid: string, signal?: AbortSignal) =>
    sendJSON<NodeConnectionDiagnostics>("POST", `/api/v1/nodes/${encodeURIComponent(uuid)}/connection-diagnostics`, undefined, signal),
};
