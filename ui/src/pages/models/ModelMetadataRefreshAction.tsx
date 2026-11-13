import { useState } from "react";
import { useTranslation } from "react-i18next";
import { ConsoleIcon } from "../../ui-kit/consoleIcons";
import { message } from "../../ui-kit/feedback";
import { api, type Model } from "../../api";
import { apiErrorText } from "../../apiError";
import { outboundPolicyApi } from "../../outboundPolicyApi";
import { OutboundHostAccess, outboundHostName, useOutboundHostAccess } from "../../components/OutboundHostAccess";
import { useIsAdmin } from "../../components/useIsAdmin";
import { Button } from "../../components/ui/button";
import { refreshRemoteModelMetadata } from "./modelMetadataRefresh";

/** Only a management-node read needs the management-node permission. */
export default function ModelMetadataRefreshAction({ model, onChanged }: {model: Model; onChanged: (model: Model) => void}) {
 const { t } = useTranslation("models");
 const { t: tCommon } = useTranslation("common");
 const admin = useIsAdmin();
 const [sources, setSources] = useState<string[]>([]);
 const access = useOutboundHostAccess(sources, sources.length > 0);
 const [refreshing, setRefreshing] = useState(false);
 const [error, setError] = useState<unknown>(null);
 const refresh = async () => {
  setRefreshing(true);
  setError(null);
  let awaitingPermission = false;
  try {
   const updated = await refreshRemoteModelMetadata(model, async (id, metadata) => {
    // Browser-provided metadata does not cause the server to read the source.
    if (metadata || !admin) return api.refreshModelMetadata(id, metadata);
    const policy = await outboundPolicyApi.get().catch(failure => {
     if (access.selected.length > 0) throw failure;
     return null;
    });
    if (!policy?.restrict_private_hosts) {
     setSources([]);
     return api.refreshModelMetadata(id);
    }
    const source = model.source.trim().toLowerCase();
    const bases = source === "url" ? [model.source_url || ""]
     : source === "huggingface" || source === "modelscope" ? (await api.metadataSources(source)).bases : [];
    const hosts = [...new Set(bases.map(outboundHostName).filter(Boolean))];
    if (hosts.length === 0) return api.refreshModelMetadata(id);
    if (sources.length > 0 && sources.join("\n") === bases.join("\n")) {
     await access.authorize();
     return api.refreshModelMetadata(id);
    }
    const checks = await outboundPolicyApi.checkHosts(hosts);
    if (checks.length === hosts.length && checks.every(item => item.status === "allowed")) {
     setSources([]);
     return api.refreshModelMetadata(id);
    }
    setSources(bases);
    awaitingPermission = true;
    return model;
   });
   if (!awaitingPermission) {
    setSources([]);
    onChanged(updated);
    message.success(t("detail.message.metadataUpdated"));
   }
  } catch (failure) { setError(failure); }
  finally { setRefreshing(false); }
 };
 return <div className={sources.length > 0 ? "w-full min-w-0 space-y-2" : "space-y-2"}>
  <Button type="button" variant="ghost" size="sm" leadingIcon={ConsoleIcon.refresh} loading={refreshing} disabled={access.pending} onClick={() => void refresh()}>
   {t("detail.refreshMetadata")}
  </Button>
  <OutboundHostAccess access={access} />
  {error != null && <p role="alert" className="text-sm text-fg-danger">{apiErrorText(tCommon, error)}</p>}
 </div>;
}
