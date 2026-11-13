import { useState } from "react";
import { Alert, Button, Card, Spin } from "antd";
import { useTranslation } from "react-i18next";
import { usePolling } from "../../api";
import { schedulingApi } from "../../schedulingApi";
import { ScopeHint, SharingTable } from "./TimelineStats";
import WindowPicker from "./WindowPicker";

const SHARING_POLL_MS = 60_000;

/**
 * SharingStats is the stats of the deployments that share cards (every
 * yield policy but never) across the whole fleet: how long each was awake,
 * how often it changed places with others and how long the requests held for
 * its wake waited. The windows are the ones the server lists (its default
 * until one is picked); the figures come from the scheduling activity's
 * rollups, not the event log.
 */
export default function SharingStats() {
  const { t } = useTranslation("scheduling");
  const [picked, setPicked] = useState<string | undefined>(undefined);
  const stats = usePolling(() => schedulingApi.timeline({ sharing: true, window: picked }), SHARING_POLL_MS, [picked]);
  const data = stats.data;
  const picker = data ? <WindowPicker windows={data.windows} value={picked ?? data.window.key} onChange={setPicked} /> : null;
  let body;
  if (!data && stats.error) {
    body = (
      <Alert type="error" showIcon message={t("sharing.loadFailed")} description={stats.error.message} action={<Button onClick={() => void stats.refetch()}>{t("page.retry")}</Button>} />
    );
  } else if (!data) {
    body = <div role="status" className="scheduling-loading"><Spin /> <span>{t("sharing.loading")}</span></div>;
  } else {
    body = (
      <Spin spinning={stats.loading}>
        {stats.error && (
          <Alert
            style={{ marginBottom: 12 }}
            type="warning"
            showIcon
            message={t("sharing.refreshFailed", { detail: stats.error.message })}
            action={<Button size="small" onClick={() => void stats.refetch()}>{t("page.retry")}</Button>}
          />
        )}
        <SharingTable rows={data.deployments} />
      </Spin>
    );
  }
  const title = <>{t("sharing.title")}<ScopeHint scope={data?.scope} /></>;
  return <Card className="scheduling-card scheduling-sharing-card" title={title} extra={picker} styles={{ body: data ? { padding: 0 } : undefined, header: { paddingInline: 12 } }}>{body}</Card>;
}
