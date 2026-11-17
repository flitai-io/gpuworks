import { VersionDrawer } from "./VersionDrawer";
import { Button } from "../../components/ui/button";
import { Switch } from "../../components/ui/switch";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../components/ui/dialog";
import CopyButton from "../../components/CopyButton";
import { ConsoleIcon } from "../../ui-kit/consoleIcons";
import { useCallback, useEffect, useRef, useState } from "react";
import { Card, Space, Table, Typography, message } from "antd";
import type { ColumnsType } from "antd/es/table";
import { useTranslation } from "react-i18next";
import { api, type ServerVersionChoice, type CatalogUpdate, type OfficialInferenceCatalogResult, type OfficialInferenceCatalogSkipped, type ServerUpgradeStarted, type UpdateStatus } from "../../api";
import { APIError, apiErrorText, productMessageText, type ProductMessage } from "../../apiError";
import { formatTime, formatTimeSec } from "../../i18n/datetime";
import { publishUpdateStatus, serverUpdateState } from "../../components/UpdateNotice";
import { Badge } from "../../components/ui/badge";
import { HelpTip } from "../../ui-kit/HelpTip";
import { Notice } from "../../ui-kit/Notice";
import { OptionSelect } from "../../ui-kit/OptionSelect";
import type { ServerBuildInfo } from "../../serverBuildInfo";
import { useConsoleUpgrade } from "./useConsoleUpgrade";
import ServerAgentNotice from "./ServerAgentNotice";
import TemplatesSkippedNotice from "./TemplatesSkippedNotice";
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
    key, name: names[key], current: value.current, latest: status.available ? value.latest : undefined,
    updateAvailable: value.update_available, compatible: value.compatible,
    checkError: value.check_error,
  });
  return [
    { key: "server", name: names.server, current: status.server.current, latest: status.available ? status.server.latest : undefined, updateAvailable: status.server.update_available, compatible: true },
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
  const checkGeneration = useRef(0);
  const check = useCallback(async (refresh = false) => {
    const generation = ++checkGeneration.current;
    setLoading(true);
    try {
      const next = await api.updates(refresh);
      if (generation !== checkGeneration.current) return;
      setStatus(next);
      publishUpdateStatus(next);
      setLoadError("");
    } catch (error) {
      if (generation === checkGeneration.current) setLoadError(apiErrorText(tCommon, error));
    } finally {
      if (generation === checkGeneration.current) setLoading(false);
    }
  }, [tCommon]);

  useEffect(() => {
    void check();
  }, [check]);

  const onUpgradeResult = useCallback((version: string, recovered: boolean) => {
    if (recovered) message.success(t("updates.serverUpgraded", { version }));
    void check();
  }, [check, t]);
  const { begin, trackDownload, attempt, summary, dismiss, checkingResult, probeVersion, lastCheck } = useConsoleUpgrade(onUpgradeResult);
  const stage = attempt?.phase;
  const waiting = stage === "download" || stage === "restarting" || stage === "unknown";
  const busy = request || (waiting ? "server" : "");

  const switchChannel = async (channel: string) => {
    checkGeneration.current++;
    setSwitchingTo(channel);
    setLoading(true);
    try {
      await api.setUpdateChannel(channel);
      setStatus(current => current && { ...current, channel, available: false, checked_at: "",
        source: undefined, unavailable_reason: undefined, sources: [] });
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
  // What the last template update left out for a newer server stays on the
  // page: a toast would be gone before it is read.
  const [skippedTemplates, setSkippedTemplates] = useState<OfficialInferenceCatalogSkipped[]>([]);
  const templatesApplied = (result: OfficialInferenceCatalogResult) => {
    setSkippedTemplates(result.skipped ?? []);
    return t("updates.templatesApplied", { version: result.version, count: result.models });
  };
  const updateTemplates = () => apply("templates", async () => templatesApplied(await api.syncInferenceTemplates()));
  const uploadTemplates = (file: File) => apply("templatesUpload", async () => templatesApplied(await api.uploadOfficialInferenceCatalog(file)));

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
  const [picker, setPicker] = useState(false);
  const [choice, setChoice] = useState("");
  const [problem, setProblem] = useState("");
  const [confirmed, setConfirmed] = useState<{ channel: string; release: ServerVersionChoice } | null>(null);
  const submitting = useRef(false);
  const openVersions = () => { setProblem(""); setChoice(""); setPicker(true); };
  const returnToPicker = () => { setConfirmed(null); setPicker(true); };
  const startSelected = async () => {
    if (!status || !confirmed || submitting.current) return;
    submitting.current = true;
    const { channel, release } = confirmed;
    const id = Array.from(crypto.getRandomValues(new Uint8Array(16)), b => b.toString(16).padStart(2, "0")).join("");
    setConfirmed(null);
    trackDownload(id, status.server.current, release.version);
    try {
      const started = await api.upgradeServer({ channel, version: release.version, sha256: release.sha256, attempt: id });
      begin(started, id);
    } catch (error) {
      if (error instanceof APIError && (error.status < 500 || ["UPDATE_DOWNLOAD_FAILED", "UPDATE_SOURCES_UNAVAILABLE"].includes(error.code))) {
        dismiss();
        setProblem(apiErrorText(tCommon, error));
        setPicker(true);
      } else {
        message.warning(apiErrorText(tCommon, error));
      }
    } finally {
      submitting.current = false;
    }
  };

  const serverState = status && serverUpdateState(status);

  const stateOf = (row: Row) => {
    if (!status?.available) return <Text type="secondary">{t("updates.unknown")}</Text>;
    if (row.checkError) return <Text type="danger">{productMessageText(tCommon, row.checkError)}</Text>;
    if (!row.latest) return <Text type="secondary">{t("updates.notPublished")}</Text>;
    if (row.key === "server" && serverState === "differentChannelVersion") return <Badge size="sm" status="neutral">{t("updates.differentChannelVersion")}</Badge>;
    if (!row.updateAvailable) return <Badge size="sm" status="success">{t("updates.upToDate")}</Badge>;
    if (!row.compatible) return <Badge size="sm" status="danger">{t("updates.needsServer")}</Badge>;
    return <Badge size="sm" status="warning">{t("updates.available")}</Badge>;
  };

  const actionsOf = (row: Row) => {
    const canApply = Boolean(status?.available && row.latest && row.updateAvailable && row.compatible);
    if (row.key === "server") {
      if (!status?.server.self_upgrade) {
        const browseVersions = <Button variant="secondary" size="sm" onClick={openVersions}>{t("versionPicker.browse")}</Button>;
        const dir = status?.server.install_dir;
        // An installed server that nothing would start again: the fix is to
        // run it behind its supervisor, which restart does on a host and the
        // container's start command does in a container.
        if (status?.server.self_upgrade_blocker === "no_service_manager" && dir) {
          const restart = `${dir}/gpuworks restart --dir ${dir}`;
          const entry = `${dir}/gpuworks serve --supervise --config ${dir}/server.yaml`;
          return (
            <Space direction="vertical" size={2}>
              {browseVersions}
              <Text type="secondary">{t("updates.blocker.no_service_manager")}</Text>
              <Text type="secondary">{t("updates.superviseHost")}</Text>
              <span className="inline-flex items-center gap-1"><Text code>{restart}</Text>
                <CopyButton size="small" text={restart} label={tCommon("gpuHealth.copyCommand")} />
              </span>
              <Text type="secondary">{t("updates.superviseContainer")}</Text>
              <span className="inline-flex items-center gap-1"><Text code>{entry}</Text>
                <CopyButton size="small" text={entry} label={tCommon("gpuHealth.copyCommand")} />
              </span>
            </Space>
          );
        }
        return (
          <Space direction="vertical" size={2}>
              {browseVersions}
            <span className="inline-flex items-center gap-1"><Text code>sudo gpuworks upgrade</Text>
              <CopyButton size="small" text="sudo gpuworks upgrade" label={tCommon("gpuHealth.copyCommand")} />
            </span>
            {status?.server.self_upgrade_blocker && <Text type="secondary">{t(`updates.blocker.${status.server.self_upgrade_blocker}`, { defaultValue: status.server.self_upgrade_blocker })}</Text>}
          </Space>
        );
      }
      return (
        <Space wrap>
          <Button size="sm" onClick={openVersions} disabled={serverState === "upToDate" || Boolean(busy) || waiting || loading}>
            {t("versionPicker.title")}
          </Button>
          <UploadButton label={t("updates.uploadServer")} accept=".tgz,.gz,application/gzip"
            disabled={busy !== "" || waiting} loading={busy === "serverUpload"}
            onFile={file => upgradeServer("serverUpload", () => api.uploadServerArchive(file))} />
        </Space>
      );
    }
    const images = row.key === "images";
    return (
      <Space wrap>
        <Button variant="secondary" size="sm" disabled={!canApply || busy !== ""}
          loading={busy === (images ? "images" : "templates")} onClick={() => void (images ? updateImages() : updateTemplates())}>
          {t("updates.apply")}
        </Button>
        <UploadButton label={images ? t("updates.uploadManifest") : t("updates.uploadPackage")}
          accept={images ? ".json,application/json" : ".tgz,.gz,application/gzip"}
          disabled={busy !== ""} loading={busy === (images ? "imagesUpload" : "templatesUpload")}
          onFile={images ? uploadImages : uploadTemplates} />
      </Space>
    );
  };

  const columns: ColumnsType<Row> = [
    { title: t("updates.updateItem"), dataIndex: "name", key: "name", render: (_, row) => <span className="gw-item-name">{row.name}</span> },
    { title: t("updates.current"), key: "current", render: (_, row) => <><span className="gw-mini-label" aria-hidden="true">{t("updates.current")}</span>{row.current || "—"}</> },
    { title: t("updates.channelLatest"), key: "latest", render: (_, row) => <><span className="gw-mini-label" aria-hidden="true">{t("updates.channelLatest")}</span>{row.latest ? <Text>{row.latest}</Text> : <Text type="secondary">—</Text>}</> },
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
  const showServerNotice = waiting || stage === "rollback" || stage === "failed" || Boolean(summary);
  const serverNotice = <div className="gw-server-notices">
    {stage === "download" && <Notice title={t("versionPicker.download",{version:attempt?.to})}>{t("versionPicker.downloadHelp")}</Notice>}
    {stage === "restarting" && <Notice title={t("versionPicker.restarting",{version:attempt?.to})}>{t("updates.serverUpgradingHelp")}</Notice>}
    {stage === "unknown" && <Notice tone="warning" title={t("versionPicker.unknown")} action={
      <Button variant="secondary" size="sm" disabled={checkingResult} loading={checkingResult} onClick={probeVersion}>
        {t("versionPicker.recheckRunning")}
      </Button>
    }>
      <div>{t("versionPicker.unknownHelp", {version:attempt?.to})}</div>
      {lastCheck && <div>{t(`versionPicker.probe.${lastCheck.reason}`, {time:formatTimeSec(lastCheck.at), version:lastCheck.version})}</div>}
    </Notice>}
    {stage === "rollback" && <Notice tone="warning" title={t("versionPicker.restored", {version:attempt?.from})}
      action={<Button variant="secondary" size="sm" onClick={dismiss}>{t("versionPicker.acknowledge")}</Button>}>
      {t("versionPicker.restoredHelp", {version:attempt?.to})}
    </Notice>}
    {stage === "failed" && <Notice tone="danger" title={t("versionPicker.failed", {version:attempt?.to})}
      action={<Button variant="secondary" size="sm" onClick={dismiss}>{t("versionPicker.acknowledge")}</Button>} />}
    {summary && <ServerAgentNotice summary={summary} dismiss={dismiss} />}
  </div>;
  return (
    <Card id="updates" classNames={{ header: "gw-card-head", body: "gw-card-body" }} title={<div className="gw-card-title">
      <span>{t("updates.title")}</span>
      <div className="gw-preferences">
        {status && (
          <Space align="center">
            <Switch id="update-auto-check" label={t("updates.autoCheck")} checked={status.auto_check}
              className="p-0" aria-busy={savingAutoCheck}
              disabled={savingAutoCheck || !status.channel_editable || busy !== "" || waiting || switchingTo !== null}
              onCheckedChange={value => void switchAutoCheck(value)} />
            <HelpTip content={status.auto_check ? t("updates.autoCheckOnHelp") : t("updates.autoCheckOffHelp")} />
          </Space>
        )}
        <UsageReportSwitch />
      </div>
    </div>}>
      <div className="gw-updates">
        <p className="gw-description">{t("updates.description")}</p>
        {!status && showServerNotice && serverNotice}
        {loadError && <Notice tone="danger" title={loadError} />}
        {status && notChecked && !loading && !loadError && <Notice tone="info" title={t("updates.notChecked")}>{t("updates.notCheckedHelp")}</Notice>}
        {status && !notChecked && !status.available && !loading && <UnavailableAlert status={status} channelName={channelName(status.channel)} />}
        <div className="gw-toolbar">
          {status && <div className="gw-channel">
            <span className="gw-channel-label">{t("updates.channel")}</span>
            <OptionSelect aria-label={t("updates.channel")} value={channel} options={channelOptions}
              disabled={!status.channel_editable || busy !== "" || waiting || switchingTo !== null}
              onChange={value => void switchChannel(value)} />
            <p className="gw-channel-help">{channel === "release" ? t("updates.channelReleaseHelp") : t("updates.channelNightlyHelp")}</p>
          </div>}
          <div className="gw-check">
            {status && (loading || !notChecked) && <span>{loading ? t("updates.checking") : t("updates.checkedAt", { time: formatTime(status.checked_at) })}</span>}
            <Button variant="secondary" leadingIcon={ConsoleIcon.refresh} loading={loading} aria-busy={loading} onClick={() => void check(true)}>{t("updates.check")}</Button>
          </div>
        </div>
        {status && <RunningBuild version={status.server.current} build={status.server.current_build} />}
        {status && <div className="gw-items-table">
          <Table<Row> rowKey="key" size="small" pagination={false} loading={loading}
            dataSource={rowsOf(status, names)} columns={columns}
            expandable={{
              showExpandColumn: false,
              expandedRowClassName: "gw-notice-row",
              expandedRowKeys: showServerNotice ? ["server"] : [],
              rowExpandable: row => row.key === "server",
              expandedRowRender: () => serverNotice,
            }} />
        </div>}
        <TemplatesSkippedNotice skipped={skippedTemplates} />

      </div>
      {status && <VersionDrawer open={picker} canInstall={status.server.self_upgrade}
        blocker={status.server.self_upgrade_blocker ? t(`updates.blocker.${status.server.self_upgrade_blocker}`, { defaultValue: status.server.self_upgrade_blocker }) : undefined}
        server={status.server} channel={status.channel} initial={choice} problem={problem} onRefresh={() => setProblem("")}
        onClose={() => setPicker(false)} onChoose={release => { setChoice(release.version); setPicker(false); setConfirmed({channel:status.channel, release}); }} />}
      <Dialog open={Boolean(confirmed)} onOpenChange={open => { if (!open) returnToPicker(); }}>
        <DialogContent size="lg" closeButtonLabel={tCommon("actions.close")}>
          <DialogHeader><DialogTitle>{t("versionPicker.confirmTitle",{version:confirmed?.release.version})}</DialogTitle></DialogHeader>
          <p className="mb-3"><strong>{status?.server.current_build?.build_channel || "—"} · {status?.server.current}</strong> → <strong>{confirmed?.channel} · {confirmed?.release.version}</strong></p>
          <p>{t("updates.serverConfirm")}</p>
          <DialogFooter>
            <Button variant="secondary" onClick={returnToPicker}>{t("versionPicker.back")}</Button>
            <Button onClick={startSelected}>{t("versionPicker.confirm")}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  );
}

function UploadButton({ label, accept, disabled, loading, onFile }: {
  label: string; accept: string; disabled: boolean; loading: boolean; onFile: (file: File) => Promise<void>;
}) {
  const input = useRef<HTMLInputElement>(null);
  return <>
    <Button variant="secondary" size="sm" leadingIcon={ConsoleIcon.upload} disabled={disabled}
      loading={loading} aria-busy={loading} onClick={() => input.current?.click()}>{label}</Button>
    <input ref={input} type="file" hidden accept={accept} aria-label={label} disabled={disabled || loading}
      onChange={event => {
        const file = event.currentTarget.files?.[0];
        event.currentTarget.value = "";
        if (file) void onFile(file);
      }} />
  </>;
}

function RunningBuild({ version, build }: { version: string; build?: ServerBuildInfo }) {
  const { t } = useTranslation("common");
  const info = build?.version === version ? build : undefined;
  const kind = info && ["stable", "prerelease", "development"].includes(info.kind) ? info.kind : "unknown";
  const tone = { stable: "success", prerelease: "warning", development: "info", unknown: "neutral" } as const;
  const { t: settings } = useTranslation("settings");
  return <section className="gw-running-build" aria-label={settings("updates.currentBuild")}>
    <div className="gw-build-identity">
      <h3>{settings("updates.currentBuild")} <HelpTip content={settings("updates.serverHelp")} /></h3>
      <div className="gw-build-id">
        <strong>{version}</strong>
        <Badge size="sm" status={tone[kind]}>{t(`buildInfo.kind.${kind}`)}</Badge>
      </div>
      <p>{t(`buildInfo.description.${kind}`)}</p>
    </div>
    <div className="gw-build-record">
      <dl className="gw-build-meta">
        {info?.build_channel && <>
          <dt>{settings("updates.buildFields.channel")}</dt>
          <dd>{info.build_channel}</dd>
        </>}
        {info?.built_at && <>
          <dt>{settings("updates.buildFields.builtAt")}</dt>
          <dd>{formatTime(info.built_at)}</dd>
        </>}
        <dt>{settings("updates.buildFields.matched")}</dt>
        <dd>{info?.published_version || t("buildInfo.unmatched")}</dd>
      </dl>
      {info?.commit && <details className="gw-build-details">
        <summary>{t("buildInfo.details")}</summary>
        <div>
          <code>{t("buildInfo.commit", { commit: info.commit })}</code>
        </div>
      </details>}
    </div>
  </section>;
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
