import { useTranslation } from "react-i18next";
import type { KubeCluster } from "../../api";
import { Badge } from "../../components/ui/badge";
import { Tooltip } from "../../components/ui/tooltip";

/**
 * Beside an entry's name: the server names the entries that may be of the
 * same cluster and use the same namespace, where the one applied later takes
 * the other's agents. Nothing when there are none.
 */
export default function KubeNamespaceShared({ cluster }: { cluster: KubeCluster }) {
  const { t } = useTranslation("kubernetes");
  if (!cluster.namespace_shared_with?.length) return null;
  return (
    <Tooltip content={t("namespaceShared.hint", { names: cluster.namespace_shared_with.join(", "), namespace: cluster.namespace })}>
      <Badge size="sm" status="warning" tabIndex={0}>{t("namespaceShared.badge")}</Badge>
    </Tooltip>
  );
}
