import { useState } from "react";
import { InfoCircleOutlined } from "@ant-design/icons";
import { Modal, Tabs, Tooltip } from "antd";

import { Button } from "../components/ui/button";
import { useTranslation } from "react-i18next";
import type { Deployment } from "../api";
import { useIsAdmin } from "../components/useIsAdmin";
import { useSchedules } from "./DeploymentSchedule";
import DeploymentInfoField, { DeploymentInfoList } from "./DeploymentInfoField";
import { policySections } from "./schedulingPolicySections";

export default function DeploymentSchedulingPolicy({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  if (dep.desired_state === "deleting") return null;
  return <PolicyCard dep={dep} onChanged={onChanged} />;
}

function PolicyCard({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const isAdmin = useIsAdmin();
  const schedules = useSchedules(dep.id);
  const [editing, setEditing] = useState(false);
  const context = { dep, schedules, onChanged };
  const sections = policySections.filter((section) => section.visible(dep));
  const editButton = isAdmin && (
    <Button className="ms-1 align-middle" variant="tertiary" size="xs" aria-label={t("schedulingPolicy.edit")} onClick={() => setEditing(true)}>
      {t("detail.edit")}
    </Button>
  );
  return (
    <section className="deployment-fields-column" aria-label={t("schedulingPolicy.title")}>
      <DeploymentInfoList>
        <DeploymentInfoField
          label={t("schedulingPolicy.title")}
          hint={<Tooltip title={t("schedulingPolicy.help")} trigger={["hover", "focus"]}>
                <InfoCircleOutlined role="img" aria-label={t("schedulingPolicy.helpLabel")} tabIndex={0} style={{ fontSize: 13, cursor: "help" }} />
              </Tooltip>}
        >
            <div>
              {sections.map(section => t("schedulingPolicy.item", {
                label: t(section.summaryLabel ?? section.label),
                value: section.summary(t, context),
              })).join(t("schedulingPolicy.separator"))}
              {isAdmin && <>{" "}{editButton}</>}
            </div>
        </DeploymentInfoField>
      </DeploymentInfoList>
      {editing && (
        <Modal
          open
          width={720}
          title={t("schedulingPolicy.title")}
          onCancel={() => setEditing(false)}
          footer={<Button variant="tertiary" size="md" onClick={() => setEditing(false)}>{t("schedulingPolicy.close")}</Button>}
        >
          <Tabs
            items={sections.map((section) => ({ key: section.key, label: t(section.label), children: section.render(context) }))}
          />
        </Modal>
      )}
    </section>
  );
}
