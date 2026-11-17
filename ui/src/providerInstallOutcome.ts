import type { CheckResult, ProviderInstallResponse } from "./api";

// What the operator needs to know after pressing Install, in one word:
//   installed           the agent runs and has connected; nothing to look at
//   installedWithNotes  the same, but some check row did not pass (the install went through)
//   waitingForAgent     the agent started and has not connected yet
//   waitingForServer    the agent started and the server cannot reach it yet (server_dial)
//   failed              nothing was installed, or the agent did not start
export type InstallOutcome = "installed" | "installedWithNotes" | "waitingForAgent" | "waitingForServer" | "failed";

export interface InstallVerdict {
  outcome: InstallOutcome;
  // The reason, for a failure or a pending connection: the request's own
  // error, or the deciding check (rendered from its code).
  detail?: string;
  check?: CheckResult;
}

// The rows that decide whether the agent is running. Other rows (the precheck
// above all) describe the host and never mean the install did not happen.
const CONNECTION_CHECKS = new Set(["agent_dial", "server_dial"]);

export function installVerdict(result: ProviderInstallResponse | null, requestError: string | null): InstallVerdict | null {
  if (requestError !== null) return { outcome: "failed", detail: requestError };
  if (!result) return null;
  const checks = result.checks ?? [];
  const started = checks.find((check) => check.id === "install_agent");
  if (started?.status === "fail") return { outcome: "failed", check: started };
  const connection = checks.find((check) => CONNECTION_CHECKS.has(check.id));
  if (connection?.status === "fail") return { outcome: "failed", check: connection };
  if (connection && connection.status !== "pass") {
    // Who connects decides what to look at: the agent's way out, or the
    // node's firewall in front of the agent's port. A connection the server
    // could not observe (skip) is not known to be up either.
    return { outcome: connection.id === "server_dial" ? "waitingForServer" : "waitingForAgent", check: connection };
  }
  // A row that did not run or found a problem is a note; na is not.
  if (checks.some((check) => check.status !== "pass" && check.status !== "na")) {
    return { outcome: "installedWithNotes" };
  }
  return { outcome: "installed" };
}
