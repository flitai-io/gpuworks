import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";
import type { KindResource, PlatformResource } from "../../api";
import { Badge } from "../../components/ui/badge";

const RESOURCE_COLORS: Record<PlatformResource, "blue" | "purple" | "gray" | "green"> = {
  vm: "blue", container: "purple", host: "gray", kubernetes: "green",
};

/** The console's word for a kind of resource: "VM", "Kubernetes". */
export function resourceTypeLabel(t: TFunction<"providers">, type: string): string {
  return t(`resource.${type}`, { defaultValue: type });
}

/** How a resource is named where space is short: by the vendor's product name for it (ECS, ACK), or without one by its kind. */
export function resourceLabel(t: TFunction<"providers">, resource: KindResource): string {
  return resource.product || resourceTypeLabel(t, resource.type);
}

/** One resource an external platform connects, as a tag. A product name says what kind it is on hover. */
export function PlatformResourceBadge({ resource }: { resource: KindResource }) {
  const { t } = useTranslation("providers");
  return (
    <Badge variant="dot" size="sm" color={RESOURCE_COLORS[resource.type] ?? "gray"} className="whitespace-nowrap"
      title={resource.product ? resourceTypeLabel(t, resource.type) : undefined}>
      {resourceLabel(t, resource)}
    </Badge>
  );
}

/** A platform's resources on one line that wraps. */
export function PlatformResourceBadges({ resources }: { resources: KindResource[] }) {
  return (
    <span className="inline-flex flex-wrap items-center gap-1">
      {resources.map((resource) => <PlatformResourceBadge key={resource.type} resource={resource} />)}
    </span>
  );
}
