import type { AgentUpgradeCreate, AgentUpgradeItem, AgentUpgradePage, AgentUpgradePreview, AgentUpgradeRun } from "../../agentUpgradesTypes";
import { getJSON, sendJSON, type List } from "../http";
import type { AgentFleetSummary, AgentRejectEvent } from "../nodes";

export const agentsApi = {
  agentRejects: () => getJSON<List<AgentRejectEvent>>("/api/v1/agent/rejects"),
  agentUpgradeErrors: () =>
    getJSON<Record<string, string>>("/api/v1/agent/upgrade-errors"),
  agentUpgradePreview: (body: { scope: "all" } | { node_uuids: string[] }) =>
    sendJSON<AgentUpgradePreview>("POST", "/api/v1/agent-upgrades/preview", body),
  createAgentUpgrade: (body: AgentUpgradeCreate) =>
    sendJSON<AgentUpgradeRun>("POST", "/api/v1/agent-upgrades", body),
  agentFleetSummary: () => getJSON<AgentFleetSummary>("/api/v1/agent-upgrades/summary"),
  agentUpgradeRuns: (page = 1, pageSize = 20) =>
    getJSON<AgentUpgradePage<AgentUpgradeRun>>(`/api/v1/agent-upgrades?page=${page}&page_size=${pageSize}`),
  agentUpgradeRun: (id: number) => getJSON<AgentUpgradeRun>(`/api/v1/agent-upgrades/${id}`),
  agentUpgradeItems: (id: number, page = 1, pageSize = 20, state = "") =>
    getJSON<AgentUpgradePage<AgentUpgradeItem>>(`/api/v1/agent-upgrades/${id}/items?page=${page}&page_size=${pageSize}${state ? `&state=${encodeURIComponent(state)}` : ""}`),
  stopAgentUpgrade: (id: number) => sendJSON<AgentUpgradeRun>("POST", `/api/v1/agent-upgrades/${id}/stop`),
  continueAgentUpgradePreview: (id: number) => sendJSON<AgentUpgradePreview>("POST", `/api/v1/agent-upgrades/${id}/continue-preview`),
};
