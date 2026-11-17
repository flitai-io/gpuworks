import { useIcons } from "../lib/icon-context";

const statusIcons = { done: "check", active: "loader", failed: "circle-x", blocked: "pause", waiting: "circle" } as const;
export type StartupStatus = keyof typeof statusIcons;

export default function StartupStatusIcon({ status }: { status: StartupStatus }) {
  const Icon = useIcons()[statusIcons[status]];
  return <Icon size={14} strokeWidth={2} aria-hidden="true" data-status={status}
    className={`deployment-startup-icon${status === "active" ? " motion-safe:animate-spin" : ""}`} />;
}
