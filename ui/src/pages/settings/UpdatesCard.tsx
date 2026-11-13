import { useCallback, useEffect, useState } from "react";
import { Button, Card, Modal, Space, Switch, Table, Typography, Upload, message } from "antd";
import { ReloadOutlined, UploadOutlined } from "@ant-design/icons";
import type { ColumnsType } from "antd/es/table";
import { useTranslation } from "react-i18next";
import { api, type CatalogUpdate, type ServerUpgradeStarted, type UpdateStatus } from "../../api";
import { apiErrorText, productMessageText, type ProductMessage } from "../../apiError";
import { formatTime } from "../../i18n/datetime";
import { publishUpdateStatus } from "../../components/UpdateNotice";
import { Badge } from "../../components/ui/badge";
import { HelpTip } from "../../ui-kit/HelpTip";
import { Notice } from "../../ui-kit/Notice";
import { OptionSelect } from "../../ui-kit/OptionSelect";
import ServerBuildSummary from "../../components/ServerBuildSummary";
import { useConsoleUpgrade } from "./useConsoleUpgrade";
import ServerAgentNotice from "./ServerAgentNotice";
import UsageReportSwitch from "./UsageReportSwitch";

const { Text } = Typography;

type Busy = "" | "images" | "imagesUpload" | "templates" | "templatesUpload" | "server" | "serverUpload";

interface Row {
  key: "server" | "images" | "templates";
  name: string;
  current: string;
  latest?: string;
  updateAvailable: boolean;
  compatible: boolean;
  checkError?: ProductMessage;
}

// What each official item looks like next to the channel: the server release,
// the system image catalog and the inference template catalog (ADR 0040).
function rowsOf(status: UpdateStatus, names: Record<Row["key"], string>): Row[] {
  const catalog = (key: "images" | "templates", value: CatalogUpdate): Row => ({
    key, name: names[key], current: value.current, latest: value.latest,
    updateAvailable: value.update_available, compatible: value.compatible,
    checkError: value.check_error,
  });
  return [
    { key: "server", name: names.server, current: status.server.current, latest: status.server.latest, updateAvailable: status.server.update_available, compatible: true },
    catalog("images", status.image_catalog),
    catalog("templates", status.template_catalog),
  ];
}

// The settings card that answers "is this installation up to date" for the
// channel the administrator picks: it applies or uploads the two official
// catalogs, and upgrades the server in place when its service manager can
// restart it (otherwise it shows the command).
export default function UpdatesCard() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const [status, setStatus] = useState<UpdateStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [request, setBusy] = useState<Busy>("");
  // switchingTo is the channel just picked, shown at once while it is saved
  // and checked; the table keeps the previous answer under a spinner.
  const [switchingTo, setSwitchingTo] = useState<string | null>(null);

  // Opening the page reuses the server's last check (at most an hour old);
  // the button and a channel switch ask the distribution channel again.
  const check = useCallback(async (refresh = false) => {
    setLoading(true);
    try {
      const next = await api.updates(refresh);
      setStatus(next);
      publishUpdateStatus(next);
      setLoadError("");
    } catch (error) {
      setLoadError(apiErrorText(tCommon, error));
    } finally {
      setLoading(false);
    }
  }, [tCommon]);

  useEffect(() => {
    void check();
  }, [check]);

  const onUpgradeResult = useCallback((version: string, recovered: boolean) => {
    if (recovered) message.success(t("updates.serverUpgraded", { version }));
    else message.error(t("updates.serverNotBack", { version }));
    void check();
  }, [check, t]);
  const { begin, upgrading, summary, dismiss } = useConsoleUpgrade(onUpgradeResult);
  const busy = request || (upgrading ? "server" : "");

  const switchChannel = async (channel: string) => {
    setSwitchingTo(channel);
    setLoading(true);
    try {
      await api.setUpdateChannel(channel);
    } catch (error) {
      setSwitchingTo(null);
      setLoading(false);
      message.error(apiErrorText(tCommon, error));
      return;
    }
    await check(true);
    setSwitchingTo(null);
  };

  // Turning automatic checks on checks at once, and with it tells the
  // channel the fleet; turning them off leaves what an earlier check found.
  const [savingAutoCheck, setSavingAutoCheck] = useState(false);
  const switchAutoCheck = async (enabled: boolean) => {
    setSavingAutoCheck(true);
    try {
      await api.setUpdateAutoCheck(enabled);
      await check();
    } catch (error) {
      message.error(apiErrorText(tCommon, error));
    } finally {
      setSavingAutoCheck(false);
    }
  };

  const apply = async (kind: Busy, action: () => Promise<string>) => {
    setBusy(kind);
    try {
      message.success(await action());
      await check();
    } catch (error) {
      message.error(apiErrorText(tCommon, error));
    } finally {
      setBusy("");
    }
  };

  const updateImages = () => apply("images", async () => {
    const result = await api.syncImages();
    return t("updates.imagesApplied", { version: result.catalog_version });
  });
  const uploadImages = (file: File) => apply("imagesUpload", async () => {
    const result = await api.importImageCatalogFile(file);
    return t("updates.imagesApplied", { version: result.catalog_version });
  });
  const updateTemplates = () => apply("templates", async () => {
    const result = await api.syncInferenceTemplates();
    return t("updates.templatesApplied", { version: result.version, count: result.models });
  });
  const uploadTemplates = (file: File) => apply("templatesUpload", async () => {
    const result = await api.uploadOfficialInferenceCatalog(file);
    return t("updates.templatesApplied", { version: result.version, count: result.models });
  });

  // A console upgrade answers once the binary is swapped; the server then
  // restarts, and the new version either answers or is rolled back.
  const upgradeServer = async (kind: Busy, start: () => Promise<ServerUpgradeStarted>) => {
    dismiss();
    setBusy(kind);
    try {
      begin(await start());
    } catch (error) {
      message.error(apiErrorText(tCommon, error));
    } finally {
      setBusy("");
    }
  };
  const confirmServerUpgrade = () => {
    Modal.confirm({
      title: t("updates.serverConfirmTitle", { version: status?.server.latest }),
      content: t("updates.serverConfirm"),
      okText: t("updates.upgrade"),
      onOk: () => void upgradeServer("server", api.upgradeServer),
    });
  };

  const stateOf = (row: Row) => {
    if (!status?.available) return <Text type="secondary">{t("updates.unknown")}</Text>;
    if (row.checkError) return <Text type="danger">{productMessageText(tCommon, row.checkError)}</Text>;
    if (!row.latest) return <Text type="secondary">{t("updates.notPublished")}</Text>;
    if (!row.updateAvailable) return <Badge size="sm" status="success">{t("updates.upToDate")}</Badge>;
    if (!row.compatible) return <Badge size="sm" status="danger">{t("updates.needsServer")}</Badge>;
    return <Badge size="sm" status="warning">{t("updates.available")}</Badge>;
  };

  const actionsOf = (row: Row) => {
    const canApply = Boolean(status?.available && row.latest && row.updateAvailable && row.compatible);
    if (row.key === "server") {
      if (!status?.server.self_upgrade) {
        const dir = status?.server.install_dir;
        // An installed server that nothing would start again: the fix is to
        // run it behind its supervisor, which restart does on a host and the
        // container's start command does in a container.
        if (status?.server.self_upgrade_blocker === "no_service_manager" && dir) {
          const restart = `${dir}/gpuworks restart --dir ${dir}`;
          const entry = `${dir}/gpuworks serve --supervise --config ${dir}/server.yaml`;
          return (
            <Space direction="vertical" size={2}>
              <Text type="secondary">{t("updates.blocker.no_service_manager")}</Text>
              <Text type="secondary">{t("updates.superviseHost")}</Text>
              <Text code copyable={{ text: restart }}>{restart}</Text>
              <Text type="secondary">{t("updates.superviseContainer")}</Text>
              <Text code copyable={{ text: entry }}>{entry}</Text>
            </Space>
          );
        }
        return (
          <Space direction="vertical" size={2}>
            <Text code copyable={{ text: "sudo gpuworks upgrade" }}>sudo gpuworks upgrade</Text>
            {status?.server.self_upgrade_blocker && <Text type="secondary">{t(`updates.blocker.${status.server.self_upgrade_blocker}`, { defaultValue: status.server.self_upgrade_blocker })}</Text>}
          </Space>
        );
      }
      const canUpgrade = canApply && status.server.installable;
      return (
        <Space wrap>
          <Button size="small" type={canUpgrade ? "primary" : "default"} disabled={!canUpgrade || busy !== ""} loading={busy === "server"} onClick={confirmServerUpgrade}>
            {t("updates.upgrade")}
          </Button>
          <Upload accept=".tgz,.gz,application/gzip" showUploadList={false}
            beforeUpload={(file) => { void upgradeServer("serverUpload", () => api.uploadServerArchive(file)); return Upload.LIST_IGNORE; }}>
            <Button size="small" icon={<UploadOutlined />} disabled={busy !== ""} loading={busy === "serverUpload"}>{t("updates.uploadServer")}</Button>
          </Upload>
        </Space>
      );
    }
    const images = row.key === "images";
    return (
      <Space wrap>
        <Button size="small" type={canApply ? "primary" : "default"} disabled={!canApply || busy !== ""}
          loading={busy === (images ? "images" : "templates")} onClick={() => void (images ? updateImages() : updateTemplates())}>
          {t("updates.apply")}
        </Button>
        <Upload accept={images ? ".json,application/json" : ".tgz,.gz,application/gzip"} showUploadList={false}
          beforeUpload={(file) => { void (images ? uploadImages(file) : uploadTemplates(file)); return Upload.LIST_IGNORE; }}>
          <Button size="small" icon={<UploadOutlined />} disabled={busy !== ""} loading={busy === (images ? "imagesUpload" : "templatesUpload")}>
            {images ? t("updates.uploadManifest") : t("updates.uploadPackage")}
          </Button>
        </Upload>
      </Space>
    );
  };

  const columns: ColumnsType<Row> = [
    { title: t("updates.item"), dataIndex: "name", key: "name" },
    { title: t("updates.current"), key: "current", render: (_, row) => row.current ? <Text>{row.current}</Text> : <Text type="secondary">—</Text> },
    { title: t("updates.latest"), key: "latest", render: (_, row) => row.latest ? <Text>{row.latest}</Text> : <Text type="secondary">—</Text> },
    { title: t("updates.state"), key: "state", render: (_, row) => stateOf(row) },
    { title: t("updates.actions"), key: "actions", render: (_, row) => actionsOf(row) },
  ];

  const names = { server: t("updates.server"), images: t("updates.images"), templates: t("updates.templates") };
  const channelOptions = [
    { value: "release", label: t("updates.channelRelease") },
    { value: "nightly", label: t("updates.channelNightly") },
  ];
  const channel = switchingTo ?? status?.channel ?? "";
  // Nobody has been asked yet: automatic checks are off and no one clicked.
  const notChecked = Boolean(status && !status.available && !status.unavailable_reason && status.sources.length === 0);
  const channelName = (value: string) => channelOptions.find((option) => option.value === value)?.label ?? value;
  return (
    <Card id="updates" title={t("updates.title")} extra={(
      <Space align="center" size={16}>
        <Button icon={<ReloadOutlined />} loading={loading} onClick={() => void check(true)}>{t("updates.check")}</Button>
        {status && (
          <Space align="center">
            <Switch id="update-auto-check" checked={status.auto_check} loading={savingAutoCheck}
              disabled={!status.channel_editable || busy !== "" || switchingTo !== null} onChange={(value) => void switchAutoCheck(value)} />
            <label htmlFor="update-auto-check"><Text>{t("updates.autoCheck")}</Text></label>
            <HelpTip content={status.auto_check ? t("updates.autoCheckOnHelp") : t("updates.autoCheckOffHelp")} />
          </Space>
        )}
        <UsageReportSwitch />
      </Space>
    )}>
      <Space direction="vertical" size={12} style={{ width: "100%" }}>
        <Text type="secondary">{t("updates.description")}</Text>
        {status && (
          <Space wrap align="center">
            <OptionSelect
              prefix={<span className="text-body">{t("updates.channel")}</span>}
              aria-label={t("updates.channel")}
              value={channel}
              options={channelOptions}
              disabled={!status.channel_editable || busy !== "" || switchingTo !== null}
              onChange={value => void switchChannel(value)}
            />
            <Text type="secondary">{channel === "release" ? t("updates.channelReleaseHelp") : t("updates.channelNightlyHelp")}</Text>
          </Space>
        )}
        {loadError && <Notice tone="danger" title={loadError} />}
        {upgrading && <Notice title={t("updates.serverUpgrading", { from: upgrading.from, to: upgrading.to })}>{t("updates.serverUpgradingHelp")}</Notice>}
        {status && !notChecked && <Text type="secondary">{loading ? t("updates.checking") : t("updates.checkedAt", { time: formatTime(status.checked_at) })}</Text>}
        {status && notChecked && !loading && <Notice tone="info" title={t("updates.notChecked")}>{t("updates.notCheckedHelp")}</Notice>}
        {status && !notChecked && !status.available && !loading && <UnavailableAlert status={status} channelName={channelName(status.channel)} />}
        {status && <section aria-label={t("updates.currentBuild")}><Text strong>{t("updates.currentBuild")}</Text> <HelpTip content={t("updates.serverHelp")} /><ServerBuildSummary version={status.server.current} build={status.server.current_build} /></section>}
        <Table<Row> rowKey="key" size="small" pagination={false} loading={loading}
          dataSource={status ? rowsOf(status, names) : []} columns={columns}
          expandable={{
            showExpandColumn: false,
            expandedRowKeys: summary ? ["server"] : [],
            rowExpandable: (row) => row.key === "server",
            expandedRowRender: () => summary && <ServerAgentNotice summary={summary} dismiss={dismiss} />,
          }} />
      </Space>
    </Card>
  );
}

// UnavailableAlert says why the channel could not be read, so an empty channel
// is not mistaken for a network problem and a proxy that rewrites answers is
// not mistaken for either.
function UnavailableAlert({ status, channelName }: { status: UpdateStatus; channelName: string }) {
  const { t } = useTranslation("settings");
  switch (status.unavailable_reason) {
    case "unpublished":
      return <Notice tone="info" title={t("updates.unpublished", { channel: channelName })}>{t("updates.unpublishedHelp")}</Notice>;
    case "invalid":
      return <Notice tone="warning" title={t("updates.invalidIndex")}>{t("updates.invalidIndexHelp")}</Notice>;
    default:
      return <Notice tone="warning" title={t("updates.unavailable")}>{t("updates.unavailableHelp")}</Notice>;
  }
}
