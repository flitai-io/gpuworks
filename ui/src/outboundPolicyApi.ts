import { getJSON, sendJSON, type List } from "./api";

export interface OutboundPolicy {
  restrict_private_hosts: boolean;
  allow_private_hosts: string[];
  editable: boolean;
  revision: string;
}

export interface OutboundHostCheck {
  host: string;
  status: "allowed" | "needs_allow" | "blocked" | "unresolved";
}

export const outboundPolicyApi = {
  get: (signal?: AbortSignal) => getJSON<OutboundPolicy>("/api/v1/server/outbound-policy", signal),
  checkHosts: (hosts: readonly string[], signal?: AbortSignal) =>
    sendJSON<List<OutboundHostCheck>>("POST", "/api/v1/server/outbound-policy/check", { hosts }, signal).then((response) => response.items),
  put: (value: Pick<OutboundPolicy, "restrict_private_hosts" | "allow_private_hosts" | "revision">) =>
    sendJSON<OutboundPolicy>("PUT", "/api/v1/server/outbound-policy", value),
};
