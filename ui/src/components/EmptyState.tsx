import { Empty, EmptyMedia, EmptyDescription, EmptyActions, EmptyIllustration, type EmptyIllustrationVariant } from "./ui/empty";
import type { ReactNode } from "react";

export type EmptyStateKind = "nodes" | "models" | "deployments" | "data";

// Zeron's built-in illustrations, so every empty list draws the same way
// and follows the theme: resource lists show the resource stack, others the
// general one.
const illustrations: Record<EmptyStateKind, Exclude<EmptyIllustrationVariant, "preview">> = {
  nodes: "resources",
  models: "resources",
  deployments: "resources",
  data: "general",
};

export default function EmptyState({
  kind = "data",
  description,
  action,
  compact = false,
}: {
  kind?: EmptyStateKind;
  description: ReactNode;
  action?: ReactNode;
  compact?: boolean;
}) {
  return (
    <Empty reason="no-data" scope={compact ? "inline" : "section"} density={compact ? "compact" : "default"}>
      <EmptyMedia variant="illustration"><EmptyIllustration variant={illustrations[kind]} /></EmptyMedia>
      <EmptyDescription>{description}</EmptyDescription>
      {action && <EmptyActions>{action}</EmptyActions>}
    </Empty>
  );
}
