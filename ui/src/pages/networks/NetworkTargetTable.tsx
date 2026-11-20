import { useState, type ComponentProps } from "react";
import type { NetworkNodeFilters } from "../../networkTypes";
import { useTranslation } from "react-i18next";
import ResourceTable from "../../components/resourceTable/ResourceTable";
import { useNetworkTargetSearch, type NetworkSearchKind, type NetworkSearchTarget } from "./useNetworkTargetSearch";

type Props<T extends NetworkSearchTarget> = Omit<ComponentProps<typeof ResourceTable<T>>, "data"> & {
  networkId: number;
  kind: NetworkSearchKind;
  searchLabel?: string;
  data: T[];
};

export default function NetworkTargetTable<T extends NetworkSearchTarget>({ networkId, kind, data, searchLabel, emptyDescription, ...table }: Props<T>) {
  const { t } = useTranslation("networks");
  const label = searchLabel ?? t(kind === "nodes" ? "detail.searchNodes" : "detail.searchAttachments");
  const [filters, setFilters] = useState<NetworkNodeFilters>();
  const { visible, searchKey, query, searchBox, ...status } = useNetworkTargetSearch(networkId, kind, data, label, filters);
  return <ResourceTable<T> {...table} {...status} data={visible} pageResetKey={searchKey} toolbar={searchBox}
    onFiltersChange={kind === "nodes" ? setFilters : undefined}
    filteredEmptyDescription={kind === "nodes" ? t("detail.noMatchingNodes") : undefined}
    emptyDescription={query ? t(kind === "nodes" ? "detail.noMatchingNodes" : "detail.noMatchingAttachments") : emptyDescription} />;
}
