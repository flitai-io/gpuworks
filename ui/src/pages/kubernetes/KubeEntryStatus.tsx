import { useTranslation } from "react-i18next";
import type { KubeCluster } from "../../api";
import CopyButton from "../../components/CopyButton";
import { Badge } from "../../components/ui/badge";
import { Button } from "../../components/ui/button";
import { Popover, PopoverContent, PopoverTitle, PopoverTrigger } from "../../components/ui/popover";
import { Tooltip } from "../../components/ui/tooltip";
import { kubeTroubleshootCommands } from "../../kubeClusters";

/** Why a cluster's agents may not have joined, and the commands that tell which. */
function StalledHelp({ cluster }: { cluster: KubeCluster }) {
  const { t } = useTranslation("kubernetes");
  const commands = kubeTroubleshootCommands(cluster.namespace);
  return (
    <div className="flex max-w-[520px] flex-col gap-2">
      <p>{t("stalled.intro")}</p>
      <div className="flex items-start gap-1">
        <pre aria-label={t("stalled.copy")} className="code-block min-w-0 flex-1 overflow-x-auto text-label">{commands}</pre>
        <CopyButton size="small" label={t("stalled.copy")} text={commands} />
      </div>
      <strong className="font-semibold text-fg-default">{t("stalled.causes")}</strong>
      <ul className="m-0 list-disc ps-5">
        {(["pull", "runtimeClass", "selector", "connect"] as const).map((key) => <li key={key}>{t(`stalled.${key}`)}</li>)}
      </ul>
    </div>
  );
}

/**
 * Where a cluster entry stands: revoked, waiting for its first node (with how
 * to find out why once the server calls it stalled), or joined.
 */
export default function KubeEntryStatus({ cluster }: { cluster: KubeCluster }) {
  const { t } = useTranslation("kubernetes");
  if (cluster.revoked) return <Badge status="danger">{t("status.revoked")}</Badge>;
  // No node has joined for longer than the server allows: say how to find
  // out why, here where the cluster's state is read.
  if (cluster.stalled) {
    return (
      // Wraps under the badge where the column is narrow.
      <span className="flex flex-wrap items-center gap-x-1 whitespace-normal">
        <Badge status="warning">{t("clusterUid.notEnrolled")}</Badge>
        <Popover>
          <PopoverTrigger render={<Button variant="link" size="sm">{t("clusterUid.troubleshoot")}</Button>} />
          <PopoverContent align="end" className="w-[560px] max-w-[calc(100vw-32px)]">
            <PopoverTitle className="mb-2">{t("stalled.title")}</PopoverTitle>
            <StalledHelp cluster={cluster} />
          </PopoverContent>
        </Popover>
      </span>
    );
  }
  if (!cluster.cluster_uid) return <Tooltip content={t("clusterUid.notEnrolledHint")}><Badge status="neutral">{t("clusterUid.notEnrolled")}</Badge></Tooltip>;
  return <Badge status="success">{t("status.active")}</Badge>;
}
