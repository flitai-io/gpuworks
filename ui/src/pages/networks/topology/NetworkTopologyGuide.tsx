import { useState, type HTMLAttributes } from "react";
import { Card, Segmented, Space, Table, Typography } from "antd";
import type { ColumnsType } from "antd/es/table";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import PageHeader from "../../../components/PageHeader";
import { ComplexDiagram, SimpleDiagram } from "./TopologyDiagrams";

type Layout = "simple" | "complex";
type Lane = "edge" | "mgmt" | "serve" | "store" | "compute" | "host";
type Kind = "listen" | "auto" | "network" | "optional";

/** A setting the diagram numbers: what kind it is and the traffic it colours. */
interface Setting {
  n: number;
  kind: Kind;
  lane: Lane;
}

const SETTINGS: Record<Layout, Setting[]> = {
  simple: [
    { n: 1, kind: "listen", lane: "edge" },
    { n: 2, kind: "auto", lane: "mgmt" },
    { n: 3, kind: "network", lane: "compute" },
  ],
  complex: [
    { n: 1, kind: "listen", lane: "edge" },
    { n: 2, kind: "listen", lane: "edge" },
    { n: 3, kind: "listen", lane: "mgmt" },
    { n: 4, kind: "auto", lane: "mgmt" },
    { n: 5, kind: "network", lane: "serve" },
    { n: 6, kind: "network", lane: "store" },
    { n: 7, kind: "network", lane: "compute" },
    { n: 8, kind: "optional", lane: "host" },
  ],
};

const LEGEND: { key: string; lane: Lane; dashed?: boolean }[] = [
  { key: "external", lane: "edge" },
  { key: "management", lane: "mgmt" },
  { key: "serving", lane: "serve" },
  { key: "storageSync", lane: "store" },
  { key: "compute", lane: "compute" },
  { key: "hostAccess", lane: "host", dashed: true },
];

const refOf = (layout: Layout, n: number) => `${layout === "simple" ? "s" : "f"}${n}`;

/**
 * Two typical deployments drawn with the traffic each network carries, and a
 * table of every setting involved: where it is set, what it does and what it
 * does not decide. Pointing at a number on the diagram or a table row
 * highlights the other.
 */
function TopologyExamples() {
  const { t } = useTranslation("networks");
  const [layout, setLayout] = useState<Layout>("simple");
  const [lit, light] = useState<string | null>(null);
  const row = (n: number, field: string) => t(`topology.${layout}.rows.${n}.${field}`);

  const columns: ColumnsType<Setting> = [
    { title: "#", key: "n", width: 48, render: (_, s) => <span className="net-topo-num">{s.n}</span> },
    {
      title: t("topology.columns.setting"),
      key: "setting",
      className: "net-topo-setting",
      render: (_, s) => <span><span className={`net-topo-tag lane-${s.lane}`}>{t(`topology.kind.${s.kind}`)}</span>{row(s.n, "name")}</span>,
    },
    { title: t("topology.columns.where"), key: "where", render: (_, s) => <Typography.Text type="secondary">{row(s.n, "where")}</Typography.Text> },
    { title: t("topology.columns.does"), key: "does", render: (_, s) => row(s.n, "does") },
    { title: t("topology.columns.not"), key: "not", render: (_, s) => <span className="net-topo-not">{row(s.n, "not")}</span> },
  ];

  const Diagram = layout === "simple" ? SimpleDiagram : ComplexDiagram;
  return (
    <Card title={t("topology.title")}>
      <div className="net-topo">
        <Typography.Text type="secondary">{t("topology.description")}</Typography.Text>
        <Space wrap size={16}>
          <Segmented<Layout>
            aria-label={t("topology.title")}
            value={layout}
            onChange={(value) => { setLayout(value); light(null); }}
            options={[
              { value: "simple", label: t("topology.simple.tab") },
              { value: "complex", label: t("topology.complex.tab") },
            ]}
          />
          <div className="net-topo-legend" aria-label={t("topology.legend.title")}>
            {LEGEND.map((item) => (
              <span key={item.key} className={`net-topo-key lane-${item.lane}`}>
                <span className={`net-topo-swatch${item.dashed ? " dash" : ""}`} />
                {t(`topology.legend.${item.key}`)}
              </span>
            ))}
          </div>
        </Space>
        <div>
          <Typography.Title level={5} style={{ marginTop: 0 }}>{t(`topology.${layout}.heading`)}</Typography.Title>
          <Typography.Text type="secondary">{t(`topology.${layout}.summary`)}</Typography.Text>
        </div>
        <div className="net-topo-diagram">
          <Diagram t={t} lit={lit} light={light} />
        </div>
        <Table<Setting>
          rowKey="n"
          size="small"
          pagination={false}
          columns={columns}
          dataSource={SETTINGS[layout]}
          scroll={{ x: 760 }}
          rowClassName={(s) => (lit === refOf(layout, s.n) ? "net-topo-lit" : "")}
          onRow={(s) => ({
            tabIndex: 0,
            onMouseEnter: () => light(refOf(layout, s.n)),
            onMouseLeave: () => light(null),
            onFocus: () => light(refOf(layout, s.n)),
            onBlur: () => light(null),
          }) as HTMLAttributes<HTMLElement>}
        />
      </div>
    </Card>
  );
}

export default function NetworkTopologyGuide() {
  const { t } = useTranslation("networks");
  return (
    <>
      <PageHeader title={t("topology.guide")} parents={[<Link to="/networks">{t("title")}</Link>]} />
      <TopologyExamples />
    </>
  );
}
