import SettingsSaveButton from "./settings/SettingsSaveButton";
import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import {
  Card,
  Form,
  InputNumber,
  Space,
  Switch,
  Tabs,
  Typography,
  message,
} from "antd";
import PageHeader from "../components/PageHeader";
import { HelpTip } from "../ui-kit/HelpTip";
import { api } from "../api";
import { defaultTerminalSettings, type TerminalSettings } from "../terminal";
import { useTranslation } from "react-i18next";
import ServerListenCard from "./settings/ServerListenCard";
import PlatformSSHKeyCard from "./settings/PlatformSSHKeyCard";
import DeploymentPolicyCard from "./settings/DeploymentPolicyCard";
import SecurityCard from "./settings/SecurityCard";
import OutboundPolicyCard, { OUTBOUND_POLICY_ANCHOR } from "./settings/OutboundPolicyCard";
import UpdatesCard from "./settings/UpdatesCard";
import MCPCard from "./settings/MCPCard";
import TelemetryCard from "./settings/TelemetryCard";
import DebugSessionCard, { DIAGNOSTICS_ANCHOR } from "./settings/DebugSessionCard";
import DiagnosticsBundleCard from "./settings/DiagnosticsBundleCard";
import PullSettingsCard from "./settings/pull/PullSettingsCard";
import BrandingCard, { BRANDING_ANCHOR } from "./settings/BrandingCard";
import { TELEMETRY_ANCHOR } from "./settings/TelemetryCard";
import { SECURITY_ANCHOR } from "./settings/SecurityCard";
import DataRetentionCard, { DATA_RETENTION_ANCHOR } from "./settings/DataRetentionCard";
import PrioritySettingsCard, { PRIORITY_ANCHOR } from "./settings/PrioritySettingsCard";
import AICard, { externalHost } from "./settings/AIConfigCard";
import ExperienceCard, { EXPERIENCE_ANCHOR } from "./settings/ExperienceCard";
import { consoleExtensions, isExtensionTab, mergeSettingsTabs, type ResolvedSettingsTab } from "../extensions";
import { FeatureSlot, useExtensionAvailability, useViewerIsAdmin } from "../extensions/shell";

const { Text } = Typography;

export { externalHost };

/**
 * SETTINGS_TABS groups the settings cards. A tab is addressed by its key as
 * the URL fragment (/settings#access); a card's own fragment
 * (/settings#security) opens the tab holding it, so links into the page keep
 * working. Console extensions' tabs (ui/src/extensions) follow these.
 */
type SettingsTab = { key: string; anchors: string[]; cards: (active: boolean) => ReactNode };
const SETTINGS_TABS: SettingsTab[] = [
  {
    key: "general",
    anchors: [BRANDING_ANCHOR, "updates", EXPERIENCE_ANCHOR],
    cards: (active) => <><BrandingCard />{active && <UpdatesCard />}<ServerListenCard /><ExperienceCard /></>,
  },
  { key: "pull", anchors: ["pull-settings"], cards: () => <PullSettingsCard /> },
  {
    key: "nodes",
    anchors: [PRIORITY_ANCHOR],
    cards: () => <><PlatformSSHKeyCard /><DeploymentPolicyCard /><PrioritySettingsCard /><OrphanCleanupCard /></>,
  },
  {
    key: "access",
    anchors: [SECURITY_ANCHOR, OUTBOUND_POLICY_ANCHOR],
    cards: () => <><SecurityCard /><OutboundPolicyCard /><TerminalCard /><MCPCard /></>,
  },
  { key: "integrations", anchors: [TELEMETRY_ANCHOR], cards: () => <><AICard /><TelemetryCard /></> },
  { key: "data", anchors: [DATA_RETENTION_ANCHOR], cards: () => <DataRetentionCard /> },
  { key: "diagnostics", anchors: [DIAGNOSTICS_ANCHOR], cards: () => <><DiagnosticsBundleCard /><DebugSessionCard /></> },
];

/** settingsTabFor returns the tab a URL fragment selects (the first by default). */
export function settingsTabFor(hash: string, tabs: readonly { key: string; anchors: string[] }[] = SETTINGS_TABS): string {
  const fragment = hash.replace(/^#/, "");
  return tabs.find((tab) => tab.key === fragment || tab.anchors.includes(fragment))?.key ?? tabs[0].key;
}

function tabLabel(t: (key: string) => string, tab: SettingsTab | ResolvedSettingsTab) {
  return isExtensionTab(tab) ? t(tab.labelKey) : t(`page.tabs.${tab.key}`);
}

function tabCards(tab: SettingsTab | ResolvedSettingsTab, active: boolean) {
  if (!isExtensionTab(tab)) return tab.cards(active);
  return <FeatureSlot availability={tab.availability} component={tab.extension.component} locked={tab.extension.locked} />;
}

export default function SettingsPage() {
  const { t } = useTranslation("settings");
  const { hash } = useLocation();
  const navigate = useNavigate();
  const availabilityOf = useExtensionAvailability();
  const isAdmin = useViewerIsAdmin();
  const tabs = useMemo(() => mergeSettingsTabs(SETTINGS_TABS, consoleExtensions(), availabilityOf, isAdmin), [availabilityOf, isAdmin]);
  const active = settingsTabFor(hash, tabs);
  // A card's fragment opens its tab; then bring the card into view once the
  // tab has rendered it (the browser's own jump ran against an empty page).
  useEffect(() => {
    const fragment = hash.replace(/^#/, "");
    if (!fragment || tabs.some((tab) => tab.key === fragment)) return;
    let frame = 0;
    const deadline = Date.now() + 2000;
    const jump = () => {
      const card = document.getElementById(fragment);
      if (card) card.scrollIntoView?.({ block: "start" });
      else if (Date.now() < deadline) frame = requestAnimationFrame(jump);
    };
    jump();
    return () => cancelAnimationFrame(frame);
  }, [hash, tabs]);
  return (
    <>
      <PageHeader title={t("page.title")} description={t("page.description")} />
      <Tabs
        className="settings-tabs"
        activeKey={active}
        onChange={(key) => navigate({ hash: key }, { replace: true })}
        items={tabs.map((tab) => ({
          key: tab.key,
          label: tabLabel(t, tab),
          children: <Space direction="vertical" size={16} style={{ width: "100%" }}>{tabCards(tab, active === tab.key)}</Space>,
        }))}
      />
    </>
  );
}

function TerminalCard() {
  const { t } = useTranslation("settings");
  const [cfg, setCfg] = useState<TerminalSettings>(defaultTerminalSettings);
  const [saving, setSaving] = useState(false);
  useEffect(() => {
    api.settings()
      .then((settings) => {
        const value = settings?.terminal as Partial<TerminalSettings> | undefined;
        if (value) setCfg((current) => ({ ...current, ...value }));
      })
      .catch(() => {});
  }, []);
  const save = async () => {
    setSaving(true);
    try {
      await api.putSetting("terminal", cfg);
      message.success(t("terminal.saved"));
    } catch (e) {
      message.error(t("error.save", { detail: (e as Error).message }));
    } finally {
      setSaving(false);
    }
  };
  const labelWithHelp = (label: string, help: string) => <span className="inline-flex items-center gap-1">{label}<HelpTip content={help} /></span>;
  const field = (label: string, help: string, value: number, min: number, max: number, set: (v: number) => void) => (
    <Form.Item label={labelWithHelp(label, help)} style={{ marginBottom: 12 }}>
      <InputNumber min={min} max={max} value={value} onChange={(v) => set(Number(v ?? min))} style={{ width: 160 }} />
    </Form.Item>
  );
  return (
    <Card title={t("terminal.title")} extra={<SettingsSaveButton loading={saving} onSave={save} />}>
      <Space direction="vertical" size={4} style={{ width: "100%", maxWidth: 720 }}>
        <Text type="secondary">{t("terminal.description")} <Text strong>{t("terminal.privacy")}</Text> <HelpTip content={t("terminal.auditHelp")} /></Text>
        <Form layout="vertical" style={{ marginTop: 8 }}>
          <Form.Item label={labelWithHelp(t("terminal.enabled"), t("terminal.enabledHelp"))} style={{ marginBottom: 12 }}>
            <Switch checked={cfg.enabled} onChange={(enabled) => setCfg((c) => ({ ...c, enabled }))} />
          </Form.Item>
          {field(t("terminal.idle"), t("terminal.idleHelp"), cfg.idle_timeout_min, 1, 1440, (v) => setCfg((c) => ({ ...c, idle_timeout_min: v })))}
          {field(t("terminal.grace"), t("terminal.graceHelp"), cfg.grace_sec, 1, 120, (v) => setCfg((c) => ({ ...c, grace_sec: v })))}
          {field(t("terminal.nodeLimit"), t("terminal.nodeLimitHelp"), cfg.max_per_node, 1, 64, (v) => setCfg((c) => ({ ...c, max_per_node: v })))}
          {field(t("terminal.globalLimit"), t("terminal.globalLimitHelp"), cfg.max_global, 1, 512, (v) => setCfg((c) => ({ ...c, max_global: v })))}
        </Form>
      </Space>
    </Card>
  );
}

function OrphanCleanupCard() {
  const { t } = useTranslation("settings");
  const [enabled, setEnabled] = useState(false);
  const [saving, setSaving] = useState(false);
  useEffect(() => {
    api.settings()
      .then((settings) => {
        setEnabled(settings?.orphan_cleanup === true);
      })
      .catch(() => {});
  }, []);
  const save = async () => {
    setSaving(true);
    try {
      await api.putSetting("orphan_cleanup", enabled);
      message.success(t("common.saved"));
    } catch (e) {
      message.error(t("error.save", { detail: (e as Error).message }));
    } finally {
      setSaving(false);
    }
  };
  return (
    <Card title={t("orphanCleanup.title")} extra={<SettingsSaveButton loading={saving} onSave={save} />}>
      <Space direction="vertical" size={4} style={{ width: "100%", maxWidth: 720 }}>
        <Text type="secondary">{t("orphanCleanup.description")}</Text>
        <Form layout="vertical" style={{ marginTop: 8 }}>
          <Form.Item label={t("orphanCleanup.enabled")} style={{ marginBottom: 0 }}>
            <Switch checked={enabled} onChange={setEnabled} />
          </Form.Item>
        </Form>
      </Space>
    </Card>
  );
}
