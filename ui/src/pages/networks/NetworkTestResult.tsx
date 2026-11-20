import { useMemo, useState, type ReactNode } from "react";
import { Button, Descriptions, Modal, Space, Table, Tag, Typography } from "antd";
import { Tabs, TabsList, TabItem, TabPanel } from "../../components/ui/tabs";
import NetworkPairMatrix, { PairCell, pairBandwidth, railOK } from "./NetworkPairMatrix";
import { useTranslation } from "react-i18next";
import type { NetworkDetail, NetworkTestPair, NetworkTestRail, NetworkTestRun, NetworkTestStatus } from "../../networkTypes";
import NetworkTestOverview from "./NetworkTestOverview";
import { RemoveHCAButton } from "./RDMAEndpoints";
import { usesRails } from "./networkDisplay";
import CardTestResult from "./CardTestResult";
import { isCardKind } from "./cardTest";
import { failureText as failureTextOf } from "./testFailure";

type T = (key: string, options?: Record<string, unknown>) => string;

const statusColor: Record<NetworkTestStatus, string> = { passed: "green", partial: "orange", failed: "red" };

/** Codes the client knows how to say; anything else shows the server's text. */
const knownCodes = new Set([
  "offline", "deleted", "no_address", "no_hca", "tool_missing", "no_roce_v2_gid", "tool_failed",
  "no_peer_rail", "listener_failed", "rpc_failed", "probe_failed", "bandwidth_failed",
]);

/** A failure in the viewer's language: the code with the node it is about and the tool's own detail. */
export function failureText(t: T, f: { code?: string; side?: string; error?: string }, node?: string) {
  return failureTextOf(t, "test.error", knownCodes, f, node);
}

/** Which node a failed rail is about, and the rail's name on that node. */
function railAtFault(pair: NetworkTestPair, rail: NetworkTestRail) {
  if (rail.side === "target" || !rail.rail) return { uuid: pair.target_uuid, name: pair.target_name, hca: rail.target_rail ?? "" };
  return { uuid: pair.source_uuid, name: pair.source_name, hca: rail.rail };
}

interface Problem {
  key: string;
  node: string;
  nodeUUID: string;
  hca?: string;
  text: string;
  pairs: NetworkTestPair[];
}

/**
 * Failures grouped by what caused them: a rail on one node, or a whole node
 * (offline, no tool, no address). This is what to fix first.
 */
function problems(t: T, run: NetworkTestRun): Problem[] {
  const out = new Map<string, Problem>();
  const add = (p: Omit<Problem, "pairs">, pair: NetworkTestPair) => {
    const seen = out.get(p.key);
    if (!seen) out.set(p.key, { ...p, pairs: [pair] });
    else if (seen.pairs[seen.pairs.length - 1] !== pair) seen.pairs.push(pair);
  };
  for (const pair of run.pairs) {
    if (pair.status === "passed") continue;
    if (pair.code || !pair.rails?.length) {
      const onTarget = pair.side === "target";
      const node = onTarget ? pair.target_name : pair.side === "source" ? pair.source_name : `${pair.source_name} → ${pair.target_name}`;
      const uuid = onTarget ? pair.target_uuid : pair.source_uuid;
      const endpoint = pair.side ? uuid : `${pair.source_uuid}>${pair.target_uuid}`;
      add({ key: `pair|${pair.code ?? pair.error}|${endpoint}`, node, nodeUUID: uuid, text: failureText(t, pair, node) || t("test.failed") }, pair);
      continue;
    }
    for (const rail of pair.rails ?? []) {
      if (railOK(rail)) continue;
      const at = railAtFault(pair, rail);
      add({ key: `rail|${at.uuid}|${at.hca}|${rail.code ?? rail.error}`, node: at.name, nodeUUID: at.uuid, hca: at.hca, text: failureText(t, rail, at.name) || t("test.failed") }, pair);
    }
  }
  return [...out.values()];
}

/** The latest run of a network: what failed, then the pairs. */
export default function NetworkTestResult({ network, run, onChanged }: {
  network: NetworkDetail;
  run: NetworkTestRun | null;
  onChanged: (detail: NetworkDetail) => void;
}) {
  const { t } = useTranslation("networks");
  if (!run) return <section className="grid gap-2">
    <h3 className="text-body font-semibold">{t("test.overview.title")}</h3>
    <p className="text-body text-fg-muted">{t("test.none")}</p>
  </section>;
  return <section className="min-w-0 space-y-4">
    <NetworkTestOverview run={run} />
    {run.status === "done" && (isCardKind(run.kind)
      ? <CardTestResult run={run} />
      : <PairResults key={run.id} network={network} run={run} onChanged={onChanged} />)}
  </section>;
}

function PairResults({ network, run, onChanged }: { network: NetworkDetail; run: NetworkTestRun; onChanged: (detail: NetworkDetail) => void }) {
  const { t } = useTranslation("networks");
  const [pair, setPair] = useState<NetworkTestPair | null>(null);
  const [open, setOpen] = useState(false);
  const openPair = (selected: NetworkTestPair) => { setPair(selected); setOpen(true); };
  const found = useMemo(() => problems(t, run), [t, run]);
  // Pairs far below the fastest one usually cross a slower link; flag them so
  // a mixed fabric is visible at a glance.
  const fastest = useMemo(() => run.pairs.reduce((max, pair) => Math.max(max, pairBandwidth(run, pair) ?? 0), 0), [run]);
  const renderPair = (p: NetworkTestPair) => {
    const gbps = pairBandwidth(run, p);
    const slow = run.bandwidth && p.status === "passed" && gbps != null && gbps > 0 && gbps < fastest / 10;
    return <PairCell t={t} pair={p} run={run} onOpen={openPair} slow={slow} failure={failureText(t, p)} />;
  };

  return (
    <div className="min-w-0 space-y-4">
      {run.kind === "rdma_tool" && <p className="text-label text-fg-muted">{t("test.matrixMean")}</p>}
      <Tabs key={run.id} defaultValue="pairs" variant="underline">
        <TabsList aria-label={t("test.result")} className="mx-0 max-w-full px-0">
          <TabItem value="pairs" label={t(run.pattern === "ring" ? "test.nodePairs" : "test.globalMatrix")} />
          <TabItem value="issues" label={t("test.problems")} badge={found.length} />
        </TabsList>
        <TabPanel value="pairs" keepMounted className="pt-3">
          {run.pattern === "ring"
            ? <PairList pairs={run.pairs} renderPair={renderPair} />
            : <NetworkPairMatrix run={run} renderPair={renderPair} />}
        </TabPanel>
        <TabPanel value="issues" className="min-h-[240px] pt-3">
          {found.length === 0 ? <p className="text-fg-muted">{t("test.noProblems")}</p> : (
            <Table<Problem>
              rowKey="key"
              size="small"
              pagination={false}
              dataSource={found}
              expandable={{ expandedRowRender: problem => <PairList pairs={problem.pairs} renderPair={renderPair} /> }}
              columns={[
                { title: t("test.problemWhere"), key: "where", render: (_, p) => <span>{p.node}{p.hca && <Typography.Text code className="ml-1.5 whitespace-nowrap">{p.hca}</Typography.Text>}</span> },
                { title: t("test.problemPairs"), dataIndex: "pairs", render: (pairs: NetworkTestPair[]) => t("test.pairsAffected", { count: pairs.length }) },
                { title: t("test.problemWhy"), dataIndex: "text" },
                {
                  title: t("table.actions"),
                  key: "actions",
                  render: (_, p) => (p.hca && usesRails(network.type)
                    ? <RemoveHCAButton network={network} hca={p.hca} nodeUUID={p.nodeUUID} nodeName={p.node} onChanged={onChanged} label={t("rail.removeShort")} />
                    : null),
                },
              ]}
            />
          )}
        </TabPanel>
      </Tabs>
      <PairDetail network={network} run={run} pair={pair} open={open} onClose={() => setOpen(false)}
        afterClose={() => setPair(null)} onChanged={(d) => { setOpen(false); onChanged(d); }} />
    </div>
  );
}

function PairList({ pairs, renderPair }: { pairs: NetworkTestPair[]; renderPair: (pair: NetworkTestPair) => ReactNode }) {
  const { t } = useTranslation("networks");
  return (
    <Table<NetworkTestPair>
      rowKey={(p) => `${p.source_uuid}>${p.target_uuid}`}
      size="small"
      pagination={pairs.length > 50 ? { defaultPageSize: 50 } : false}
      dataSource={pairs}
      columns={[
        { title: t("test.source"), dataIndex: "source_name" },
        { title: t("test.target"), dataIndex: "target_name" },
        { title: t("test.result"), key: "result", render: (_, p) => renderPair(p) },
      ]}
    />
  );
}

/** One pair: its endpoints, and rail by rail for RDMA, with a way to drop a failing rail. */
function PairDetail({ network, run, pair, open, onClose, afterClose, onChanged }: {
  network: NetworkDetail;
  run: NetworkTestRun;
  pair: NetworkTestPair | null;
  open: boolean;
  onClose: () => void;
  afterClose: () => void;
  onChanged: (detail: NetworkDetail) => void;
}) {
  const { t } = useTranslation("networks");
  if (!pair) return null;
  const rails = pair.rails ?? [];
  const gbps = pairBandwidth(run, pair);
  const pairFailure = failureText(t, pair);
  return (
    <Modal open={open} title={`${pair.source_name} → ${pair.target_name}`} onCancel={onClose}
      afterClose={afterClose} footer={null} width={860} destroyOnHidden>
      <Descriptions column={1} size="small" style={{ marginBottom: 12 }}>
        <Descriptions.Item label={t("test.result")}><Tag color={statusColor[pair.status]}>{t(`test.status.${pair.status}`)}</Tag></Descriptions.Item>
        {pair.target && <Descriptions.Item label={t("test.targetAddress")}><Typography.Text code>{pair.target}</Typography.Text></Descriptions.Item>}
        {pair.reachable && pair.rtt_ms_p50 > 0 && <Descriptions.Item label="RTT">{t("test.rtt", { p50: pair.rtt_ms_p50.toFixed(2), p99: pair.rtt_ms_p99.toFixed(2) })}</Descriptions.Item>}
        {run.bandwidth && gbps != null && gbps > 0 && <Descriptions.Item label={t(run.kind === "rdma_tool" ? "test.meanBandwidth" : "test.bandwidth")}>{t("test.gbps", { value: gbps.toFixed(1) })}</Descriptions.Item>}
        {pairFailure && <Descriptions.Item label={t("test.problemWhy")}>{pairFailure}</Descriptions.Item>}
      </Descriptions>
      {rails.length > 0 && (
        <Table<NetworkTestRail>
          rowKey={(r) => `${r.rail}>${r.target_rail ?? ""}`}
          size="small"
          pagination={false}
          dataSource={rails}
          scroll={{ x: true }}
          columns={[
            { title: t("test.sourceHCA"), key: "source", render: (_, r) => (r.rail ? <Space size={6}><Typography.Text code className="whitespace-nowrap">{r.rail}</Typography.Text><Typography.Text type="secondary">{r.source_ip || "—"}</Typography.Text></Space> : "—") },
            { title: t("test.targetHCA"), key: "target", render: (_, r) => (r.target_rail ? <Space size={6}><Typography.Text code className="whitespace-nowrap">{r.target_rail}</Typography.Text><Typography.Text type="secondary">{r.target_ip || "—"}</Typography.Text></Space> : "—") },
            {
              title: t("test.result"),
              key: "result",
              render: (_, r) => (railOK(r)
                ? <Tag color="green">{t("test.gbps", { value: r.gbps.toFixed(1) })}</Tag>
                : <Space size={4} wrap><Tag color="red">{t("test.failed")}</Tag><Typography.Text>{failureText(t, r, railAtFault(pair, r).name)}</Typography.Text></Space>),
            },
            {
              title: t("table.actions"),
              key: "actions",
              render: (_, r) => {
                if (railOK(r) || !usesRails(network.type)) return null;
                const at = railAtFault(pair, r);
                return at.hca ? <RemoveHCAButton network={network} hca={at.hca} nodeUUID={at.uuid} nodeName={at.name} onChanged={onChanged} label={t("rail.removeShort")} /> : null;
              },
            },
          ]}
        />
      )}
      {rails.length > 0 && <Typography.Paragraph type="secondary" style={{ marginTop: 12, marginBottom: 0 }}>{t("test.retestHint")}</Typography.Paragraph>}
      <Space style={{ marginTop: 12, width: "100%", justifyContent: "flex-end" }}><Button onClick={onClose}>{t("actions.close")}</Button></Space>
    </Modal>
  );
}
