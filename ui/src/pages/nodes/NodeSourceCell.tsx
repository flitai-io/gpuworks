import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { NodeSource } from "../../api";
import { platformPath } from "../../nodeSources";

/**
 * Where a node came from, as the node list shows it: the external platform it
 * joined through, linked to that platform's row, with what the node is there
 * underneath (an instance, or a node of one of its clusters). A node added by
 * hand says so, and so does one whose source the server could not read.
 */
export default function NodeSourceCell({ source, unknown = false }: { source?: NodeSource; unknown?: boolean }) {
  const { t } = useTranslation("nodes");
  const { t: tProviders } = useTranslation("providers");
  // A source the server could not read is not a node added by hand.
  if (!source) return <span className="text-fg-muted">{t(unknown ? "source.unknown" : "source.manual")}</span>;
  const kind = source.provider_kind ? tProviders(`kind.${source.provider_kind}`, { defaultValue: source.provider_kind }) : "";
  // A cluster connected on its own is its own platform; one connected from an account is listed under the account.
  const name = source.provider_id ? source.provider_name || kind : source.cluster_name || t("source.standaloneCluster");
  const what = source.cluster_uuid
    ? source.provider_id ? t("source.cluster", { name: source.cluster_name }) : t("source.standaloneCluster")
    : source.resource ? tProviders(`resource.${source.resource}`, { defaultValue: source.resource }) : "";
  return (
    <>
      <Link className="text-fg-brand hover:underline" to={platformPath(source)}>{name}</Link>
      <span className="resource-table-sub">{[source.provider_id ? kind : "", what].filter(Boolean).join(" · ")}</span>
    </>
  );
}
