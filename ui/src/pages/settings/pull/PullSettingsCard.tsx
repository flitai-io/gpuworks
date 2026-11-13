import { Card, Tabs, Typography } from "antd";
import { useTranslation } from "react-i18next";
import PullProxies from "./PullProxies";
import PullSourceRules from "./PullSourceRules";
import Credentials from "./Credentials";
import SyncGroups from "./SyncGroups";
import { HelpTip } from "../../../ui-kit/HelpTip";

/**
 * PullSettingsCard is where an operator says how nodes get images and models:
 * source rules, the proxies nodes go out through, sync groups (shared by
 * images and models) and credentials. A
 * deployment carries no source of its own; every node reads these when it
 * pulls (ADR 0048).
 */
export default function PullSettingsCard() {
  const { t } = useTranslation("settings");
  return (
    <Card id="pull-settings" title={t("pull.title")}>
      <Typography.Paragraph type="secondary">{t("pull.description")} <HelpTip content={t("pull.descriptionHelp")} /></Typography.Paragraph>
      <Tabs
        destroyInactiveTabPane
        items={[
          { key: "rules", label: t("pull.tabs.rules"), children: <PullSourceRules /> },
          { key: "proxies", label: t("pull.tabs.proxies"), children: <PullProxies /> },
          { key: "groups", label: t("pull.tabs.groups"), children: <SyncGroups /> },
          { key: "credentials", label: t("pull.tabs.credentials"), children: <Credentials /> },
        ]}
      />
    </Card>
  );
}
