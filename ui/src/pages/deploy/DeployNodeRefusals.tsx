import { Alert, Collapse, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { nodeConditionSummary, type NodeRefusal } from "./deployPlacementRefusal";
import ModelDiskRegistrationLink from "./ModelDiskRegistrationLink";

const SHOWN = 5;
function RefusalList({ items }: { items: NodeRefusal[] }) {
  return <ul className="deploy-node-refusals" style={{ margin: 0, paddingInlineStart: 18 }}>
    {items.map((item) => <li key={item.uuid}><strong>{item.name}</strong>{item.reason ? `: ${item.reason}` : ""}{item.modelId && <> <ModelDiskRegistrationLink modelId={item.modelId} /></>}</li>)}
  </ul>;
}

export default function DeployNodeRefusals({ refusals, conditions }: {
  refusals: NodeRefusal[];
  conditions: ReturnType<typeof nodeConditionSummary>;
}) {
  const { t } = useTranslation("deploy");
  const matches = new Set(conditions?.matched.map((node) => node.uuid));
  const matched = refusals.filter((node) => matches.has(node.uuid));
  const primary = matched.length ? matched : refusals;
  const others = matched.length ? refusals.filter((node) => !matches.has(node.uuid)) : [];
  const more = primary.slice(SHOWN);
  return <Alert type="warning" showIcon message={t("workbench.node.noNodeFits")} description={<>
    {conditions && <Typography.Paragraph style={{ marginBottom: 8 }}>
      {t(conditions.matched.length === 0 ? "workbench.node.conditionsEmpty"
        : conditions.hardwareOnly ? "workbench.node.conditionsHardwareRejected" : "workbench.node.conditionsFurtherRejected")}
    </Typography.Paragraph>}
    <RefusalList items={primary.slice(0, SHOWN)} />
    {(more.length > 0 || others.length > 0) && <Collapse ghost size="small" items={[
      ...(more.length ? [{ key: "more", label: t("workbench.node.moreRefused", { count: more.length }), children: <RefusalList items={more} /> }] : []),
      ...(others.length ? [{ key: "others", label: t("workbench.node.otherRefused", { count: others.length }), children: <RefusalList items={others} /> }] : []),
    ]} />}
  </>} />;
}
