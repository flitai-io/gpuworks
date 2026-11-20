import { useMemo, useState } from "react";
import { Alert, Button, Descriptions, Modal, Space, Table, Tag, Tooltip, Typography } from "antd";
import type { ColumnsType } from "antd/es/table";
import { useTranslation } from "react-i18next";
import type { CardShape, NetworkTestPair, NetworkTestRail, NetworkTestRun } from "../../networkTypes";
import { cardOK } from "./cardTest";
import { failureText } from "./testFailure";

type T = (key: string, options?: Record<string, unknown>) => string;

const shapeColor: Record<CardShape, string> = { full: "green", diagonal: "blue", mixed: "orange", none: "red", untested: "red" };
const verdictType: Record<CardShape, "success" | "info" | "warning" | "error"> = { full: "success", diagonal: "info", mixed: "warning", none: "error", untested: "error" };

/** The shape the server read for a node pair; a run stored by an older server has none. */
function shapeOf(pair: NetworkTestPair): CardShape {
  return pair.card_shape ?? "untested";
}

const knownCodes = new Set(["offline", "deleted", "no_address", "no_hca", "unreachable", "no_card", "tool_failed", "no_peer_rail", "rpc_failed"]);

function cardFailure(t: T, f: { code?: string; side?: string; error?: string }, node?: string) {
  return failureText(t, "test.card.error", knownCodes, f, node);
}

function pairNode(pair: NetworkTestPair, side?: string) {
  return side === "target" ? pair.target_name : side === "source" ? pair.source_name : undefined;
}

/**
 * The latest card test of an accelerator network. It answers one question
 * first, in words: which cards can be used together. The node pairs and each
 * pair's cards are the evidence below it.
 */
export default function CardTestResult({ run }: { run: NetworkTestRun }) {
  const { t } = useTranslation("networks");
  const [open, setOpen] = useState<NetworkTestPair | null>(null);
  // The verdict is the server's reading of this run; what is left here is to
  // name the node pairs that differ from it.
  const verdict = run.card_shape;
  const oddPairs = useMemo(() => (verdict ? run.pairs.filter((p) => shapeOf(p) !== verdict) : []), [run, verdict]);
  const matrix = run.kind === "card_matrix";
  const odd = oddPairs.map((p) => `${p.source_name} ↔ ${p.target_name}`);
  const shown = odd.slice(0, 5).join(t("test.card.listSeparator"));
  return (
    <Space direction="vertical" size={16} style={{ width: "100%" }}>
      <div>
        {verdict && (
          <Alert
            style={{ marginBottom: 16 }}
            type={odd.length > 0 && verdictType[verdict] === "success" ? "warning" : verdictType[verdict]}
            showIcon
            message={t(`test.card.verdict.${matrix ? "matrix" : "ping"}.${verdict}`)}
            description={odd.length > 0 ? <span className="text-fg-default">{t("test.card.odd", { count: odd.length, pairs: shown + (odd.length > 5 ? t("test.card.more", { count: odd.length - 5 }) : "") })}</span> : undefined}
          />
        )}
        {run.pairs.length === 1
          ? <CardPairView run={run} pair={run.pairs[0]} />
          : run.pattern === "ring"
            ? <ShapeList run={run} onOpen={setOpen} />
            : <ShapeOverview run={run} onOpen={setOpen} />}
        {run.pairs.length > 1 && <ShapeLegend matrix={matrix} />}
        <p className="mt-2 text-label text-fg-muted">{t("test.card.measuredAt")}</p>
      </div>
      {open && (
        <Modal open title={`${open.source_name} ↔ ${open.target_name}`} onCancel={() => setOpen(null)} footer={null} width={860} destroyOnHidden>
          <CardPairView run={run} pair={open} />
          <Space style={{ marginTop: 12, width: "100%", justifyContent: "flex-end" }}><Button onClick={() => setOpen(null)}>{t("actions.close")}</Button></Space>
        </Modal>
      )}
    </Space>
  );
}

function shapeLabel(t: T, matrix: boolean, pair: NetworkTestPair) {
  const shape = shapeOf(pair);
  const probed = (pair.rails ?? []).filter((r) => r.rail && r.target_rail);
  const label = t(`test.card.shape.${matrix ? "matrix" : "ping"}.${shape}`);
  return shape === "mixed" ? `${label} ${probed.filter(cardOK).length}/${probed.length}` : label;
}

function ShapeTag({ run, pair, onOpen }: { run: NetworkTestRun; pair: NetworkTestPair; onOpen: () => void }) {
  const { t } = useTranslation("networks");
  const label = shapeLabel(t, run.kind === "card_matrix", pair);
  const why = cardFailure(t, pair, pairNode(pair, pair.side));
  return (
    <Tooltip trigger={["hover", "focus"]} title={why || undefined}>
      <Tag
        role="button"
        tabIndex={0}
        color={shapeColor[shapeOf(pair)]}
        style={{ cursor: "pointer" }}
        aria-label={`${pair.source_name} ↔ ${pair.target_name}: ${label}`}
        onClick={onOpen}
        onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); onOpen(); } }}
      >
        {label}
      </Tag>
    </Tooltip>
  );
}

function ShapeLegend({ matrix }: { matrix: boolean }) {
  const { t } = useTranslation("networks");
  const shapes: CardShape[] = matrix ? ["full", "diagonal", "mixed", "none"] : ["full", "mixed", "none"];
  return (
    <Space size={4} wrap style={{ marginTop: 12 }}>
      {shapes.map((shape) => <Tag key={shape} color={shapeColor[shape]}>{t(`test.card.shape.${matrix ? "matrix" : "ping"}.${shape}`)}</Tag>)}
      <Typography.Text type="secondary">{t("test.card.openHint")}</Typography.Text>
    </Space>
  );
}

/** Every node pair of a mesh, one cell each; a pair is probed once, so the cell is the same on both sides of the diagonal. */
function ShapeOverview({ run, onOpen }: { run: NetworkTestRun; onOpen: (p: NetworkTestPair) => void }) {
  const { t } = useTranslation("networks");
  const nodes = useMemo(() => {
    const seen = new Map<string, string>();
    for (const p of run.pairs) {
      seen.set(p.source_uuid, p.source_name);
      seen.set(p.target_uuid, p.target_name);
    }
    return [...seen.entries()].map(([uuid, name]) => ({ uuid, name })).sort((a, b) => a.name.localeCompare(b.name));
  }, [run]);
  const byKey = new Map<string, NetworkTestPair>();
  for (const p of run.pairs) {
    byKey.set(`${p.source_uuid}>${p.target_uuid}`, p);
    byKey.set(`${p.target_uuid}>${p.source_uuid}`, p);
  }
  const columns: ColumnsType<{ uuid: string; name: string }> = [
    { title: t("test.card.overview"), dataIndex: "name", fixed: "left" },
    ...nodes.map((other) => ({
      title: other.name,
      key: other.uuid,
      align: "center" as const,
      render: (_: unknown, node: { uuid: string }) => {
        if (node.uuid === other.uuid) return <Typography.Text type="secondary">—</Typography.Text>;
        const pair = byKey.get(`${node.uuid}>${other.uuid}`);
        return pair ? <ShapeTag run={run} pair={pair} onOpen={() => onOpen(pair)} /> : null;
      },
    })),
  ];
  return <Table rowKey="uuid" size="small" pagination={false} columns={columns} dataSource={nodes} scroll={{ x: true }} />;
}

function ShapeList({ run, onOpen }: { run: NetworkTestRun; onOpen: (p: NetworkTestPair) => void }) {
  const { t } = useTranslation("networks");
  return (
    <Table<NetworkTestPair>
      rowKey={(p) => `${p.source_uuid}>${p.target_uuid}`}
      size="small"
      pagination={run.pairs.length > 50 ? { defaultPageSize: 50 } : false}
      dataSource={run.pairs}
      columns={[
        { title: t("test.source"), dataIndex: "source_name" },
        { title: t("test.target"), dataIndex: "target_name" },
        { title: t("test.result"), key: "result", render: (_, p) => <ShapeTag run={run} pair={p} onOpen={() => onOpen(p)} /> },
      ]}
    />
  );
}

/** One node pair: a grid of its cards for a matrix, a list for same-named cards. */
function CardPairView({ run, pair }: { run: NetworkTestRun; pair: NetworkTestPair }) {
  const { t } = useTranslation("networks");
  const rails = pair.rails ?? [];
  const why = cardFailure(t, pair, pairNode(pair, pair.side));
  return (
    <>
      <Descriptions column={1} size="small" style={{ marginBottom: 12 }}>
        <Descriptions.Item label={t("test.result")}><Tag color={shapeColor[shapeOf(pair)]}>{shapeLabel(t, run.kind === "card_matrix", pair)}</Tag></Descriptions.Item>
        {why && <Descriptions.Item label={t("test.problemWhy")}>{why}</Descriptions.Item>}
      </Descriptions>
      {rails.length > 0 && (run.kind === "card_matrix" ? <CardGrid pair={pair} /> : <CardList pair={pair} />)}
    </>
  );
}

function CardCell({ pair, rail }: { pair: NetworkTestPair; rail?: NetworkTestRail }) {
  const { t } = useTranslation("networks");
  if (!rail) return <Typography.Text type="secondary">—</Typography.Text>;
  const ok = cardOK(rail);
  const text = ok ? t("test.card.rtt", { value: (rail.rtt_ms ?? 0).toFixed(2) }) : cardFailure(t, rail, pairNode(pair, rail.side));
  const label = `${rail.rail} → ${rail.target_rail}: ${ok ? t("test.reachable") : t("test.card.unreachable")}`;
  return (
    <Tooltip trigger={["hover", "focus"]} title={`${rail.source_ip ?? ""} → ${rail.target_ip ?? ""} · ${text}`}>
      <Tag tabIndex={0} color={ok ? "green" : "default"} aria-label={label} style={{ marginInlineEnd: 0 }}>{ok ? "✓" : "✗"}</Tag>
    </Tooltip>
  );
}

function CardGrid({ pair }: { pair: NetworkTestPair }) {
  const probed = (pair.rails ?? []).filter((r) => r.rail && r.target_rail);
  const sources = [...new Set(probed.map((r) => r.rail))];
  const targets = [...new Set(probed.map((r) => r.target_rail as string))];
  const byKey = new Map(probed.map((r) => [`${r.rail}>${r.target_rail}`, r]));
  const columns: ColumnsType<{ card: string }> = [
    { title: `${pair.source_name} \\ ${pair.target_name}`, dataIndex: "card", fixed: "left", render: (v: string) => <Typography.Text code className="whitespace-nowrap">{v}</Typography.Text> },
    ...targets.map((target) => ({
      title: <Typography.Text code className="whitespace-nowrap">{target}</Typography.Text>,
      key: target,
      align: "center" as const,
      render: (_: unknown, row: { card: string }) => <CardCell pair={pair} rail={byKey.get(`${row.card}>${target}`)} />,
    })),
  ];
  return <Table rowKey="card" size="small" pagination={false} columns={columns} dataSource={sources.map((card) => ({ card }))} scroll={{ x: true }} />;
}

function CardList({ pair }: { pair: NetworkTestPair }) {
  const { t } = useTranslation("networks");
  return (
    <Table<NetworkTestRail>
      rowKey={(r) => `${r.rail}>${r.target_rail ?? ""}`}
      size="small"
      pagination={false}
      dataSource={pair.rails ?? []}
      scroll={{ x: true }}
      columns={[
        { title: pair.source_name, key: "source", render: (_, r) => (r.rail ? <Space size={6}><Typography.Text code className="whitespace-nowrap">{r.rail}</Typography.Text><Typography.Text type="secondary">{r.source_ip || "—"}</Typography.Text></Space> : "—") },
        { title: pair.target_name, key: "target", render: (_, r) => (r.target_rail ? <Space size={6}><Typography.Text code className="whitespace-nowrap">{r.target_rail}</Typography.Text><Typography.Text type="secondary">{r.target_ip || "—"}</Typography.Text></Space> : "—") },
        {
          title: t("test.result"),
          key: "result",
          render: (_, r) => (cardOK(r)
            ? <Tag color="green">{t("test.card.rtt", { value: (r.rtt_ms ?? 0).toFixed(2) })}</Tag>
            : <Space size={4} wrap><Tag color="red">{t("test.card.unreachable")}</Tag><Typography.Text>{cardFailure(t, r, pairNode(pair, r.side))}</Typography.Text></Space>),
        },
      ]}
    />
  );
}
