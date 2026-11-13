import { useState } from "react";
import { Space, Tag, Typography, message } from "antd";
import { Button } from "../components/ui/button";
import { useTranslation } from "react-i18next";
import { api, type Deployment, type UnitsPlacementMode } from "../api";
import { ConfirmOrderModal } from "./OrderDialogs";

// PlacementModeControl shows who places a multi-node or PD deployment's units
// - the platform (automatic: it picks a unit's nodes and cards whenever the
// unit starts) or the operator (manual: the nodes are fixed) - and switches
// it. The switch restarts nothing; the confirmation says what changes. A
// single-node deployment states its placement in its node requirements and has
// no mode here.
export default function PlacementModeControl({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const [target, setTarget] = useState<UnitsPlacementMode | null>(null);
  const mode = dep.placement_mode;
  if (!mode || dep.desired_state === "deleting") return mode ? <PlacementTag mode={mode} /> : null;
  const next: UnitsPlacementMode = mode === "auto" ? "manual" : "auto";
  return (
    <>
      <Space direction="vertical" size={4}>
        <Space size={4} wrap>
          <PlacementTag mode={mode} />
          {mode === "auto" && dep.placement_strategy && (
            <Tag>{t(`detail.placement.strategy.${dep.placement_strategy}`, { defaultValue: dep.placement_strategy })}</Tag>
          )}
          <Button variant="tertiary" size="sm" onClick={() => setTarget(next)}>
            {t(next === "auto" ? "unitsPlacement.toAuto" : "unitsPlacement.toManual")}
          </Button>
        </Space>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>{t(`unitsPlacement.${mode}Hint`)}</Typography.Text>
      </Space>
      <ConfirmOrderModal
        open={target != null}
        title={t(next === "auto" ? "unitsPlacement.confirmTitleAuto" : "unitsPlacement.confirmTitleManual")}
        okText={t(next === "auto" ? "unitsPlacement.toAuto" : "unitsPlacement.toManual")}
        failKey="unitsPlacement.switchFailed"
        order={() => api.setPlacementMode(dep.id, next)}
        onClose={() => setTarget(null)}
        onDone={() => {
          message.success(t(next === "auto" ? "unitsPlacement.switchedAuto" : "unitsPlacement.switchedManual"));
          onChanged();
        }}
      >
        <Typography.Paragraph>
          {t(next === "auto" ? "unitsPlacement.confirmAuto" : "unitsPlacement.confirmManual")}
        </Typography.Paragraph>
      </ConfirmOrderModal>
    </>
  );
}

function PlacementTag({ mode }: { mode: UnitsPlacementMode }) {
  const { t } = useTranslation("deployments");
  return <Tag color={mode === "auto" ? "blue" : undefined}>{t(`unitsPlacement.${mode}`)}</Tag>;
}
