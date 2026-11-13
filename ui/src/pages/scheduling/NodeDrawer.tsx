import { useTranslation } from "react-i18next";
import { usePolling } from "../../api";
import { schedulingApi } from "../../schedulingApi";
import { Badge } from "../../components/ui/badge";
import { Button } from "../../components/ui/button";
import { Container, ContainerHeader, ContainerBody } from "../../components/ui/container";
import { Notice } from "../../ui-kit/Notice";
import { SideDrawer } from "../../ui-kit/SideDrawer";
import { Spinner } from "../../ui-kit/Spinner";
import GpuResidencyTable from "./GpuResidencyTable";
import ResidencyTimeline from "./ResidencyTimeline";

export default function NodeDrawer({ uuid, name, onClose }: { uuid: string | null; name: string; onClose: () => void }) {
  const opensWithDetail = /^#gpu-\d+-residency$/.test(window.location.hash);
  return <SideDrawer open={!!uuid} title={name} onClose={onClose} width={1020} className={opensWithDetail ? "node-review residency-direct" : "node-review"}>
    {uuid && <NodeDetail key={uuid} uuid={uuid}/>}
  </SideDrawer>;
}

function NodeDetail({ uuid }: { uuid: string }) {
  const { t } = useTranslation("scheduling");
  const board = usePolling(() => schedulingApi.board({ node: uuid }), 10000, [uuid]);
  const node = board.data?.nodes.items.find(item => item.uuid === uuid);
  const retry = <Button size="sm" onClick={() => void board.refetch()}>{t("page.retry")}</Button>;
  if (!node) {
    if (board.error || board.data) return <Notice tone="danger" title={t("drawer.loadFailed")} action={retry}>
      {board.error?.message ?? t("drawer.notFound")}
    </Notice>;
    return <Spinner label={t("drawer.loading")}/>;
  }
  return <>
    {board.error && <Notice tone="warning" title={t("page.refreshFailed", { detail: board.error.message })} action={retry}/>}
    <section aria-label={node.name}>
      <Container className="memory-chart rounded-2xl">
        <ContainerHeader className="py-2">
          <h2 className="text-title font-semibold">{t("nodeChart.memory")}</h2>
          <div className="flex items-center gap-2">
            <Badge status={node.online ? "success" : "neutral"} variant="dot" size="sm">{t(node.online ? "nodes.online" : "nodes.offline")}</Badge>
            <span className="text-label text-fg-muted">{t("nodeChart.gpuCount", { count: node.gpus.length })}</span>
          </div>
        </ContainerHeader>
        <ContainerBody className="rounded-xl p-0"><GpuResidencyTable node={node}/></ContainerBody>
      </Container>
    </section>
    <ResidencyTimeline nodes={[node]}/>
  </>;
}
