import type { TFunction } from "i18next";
import type { Node, PartitionCapability } from "../api";

/** The localized reason an unavailable capability is not ready, from its code. */
export function partitionReasonText(t: TFunction, capability: PartitionCapability): string | undefined {
  if (capability.status === "ready") return undefined;
  if (!capability.reason_code) return capability.reason;
  const params = capability.reason_params ?? {};
  return t(`partitionCapability.reason.${capability.reason_code}`, {
    defaultValue: capability.reason,
    runtime: params.runtime ?? "",
    supported_runtimes: (params.supported_runtimes ?? []).join(", "),
    missing_assets: (params.missing_assets ?? []).join(", "),
    gpu_index: params.gpu_index ?? "",
  });
}

export function capabilityAllowsPartition(capability?: PartitionCapability | null): boolean {
  return capability?.status === "ready";
}

export function domesticPartitionCapability(node?: Node | null): PartitionCapability | undefined {
  return node?.partition_capabilities?.find((capability) => capability.vendor !== "nvidia");
}

export function readyPartitionCapability(node?: Node | null): PartitionCapability | undefined {
  const capabilities = node?.partition_capabilities ?? [];
  return capabilities.find((capability) => capability.vendor !== "nvidia" && capabilityAllowsPartition(capability))
    ?? capabilities.find((capability) => capabilityAllowsPartition(capability));
}

export function nodeSupportsPartition(node?: Node | null): boolean {
  return Boolean(readyPartitionCapability(node));
}
