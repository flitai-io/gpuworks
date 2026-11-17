import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { SideDrawer } from "../../ui-kit/SideDrawer";
import { Notice } from "../../ui-kit/Notice";
import { Button } from "../../components/ui/button";
import { Input } from "../../components/ui/input";
import { Badge } from "../../components/ui/badge";
import { DetailList, DetailListItem, DetailListLabel, DetailListValue } from "../../components/ui/detail-list";
import { RadioGroup, RadioGroupItem } from "../../components/ui/radio-group";
import { api, type ServerVersionChoice, type UpdateStatus } from "../../api";
import { formatTime } from "../../i18n/datetime";
import { apiErrorText } from "../../apiError";

const recentLimit = 5;

interface VersionDrawerProps {
  open: boolean;
  canInstall: boolean;
  blocker?: string;
  channel: string;
  server: UpdateStatus["server"];
  initial: string;
  problem: string;
  onRefresh: () => void;
  onClose: () => void;
  onChoose: (choice: ServerVersionChoice) => void;
}

export function VersionDrawer({ open, canInstall, blocker, channel, server, initial, problem, onRefresh, onClose, onChoose }: VersionDrawerProps) {
  const { t } = useTranslation("settings");
  const [rows, setRows] = useState<ServerVersionChoice[]>([]);
  const [expanded, setExpanded] = useState(false);
  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState("");
  const [loading, setLoading] = useState(true);
  const [failed, setFailed] = useState("");
  const [platform, setPlatform] = useState("");
  const { t: common } = useTranslation("common");
  const [revision, setRevision] = useState(0);

  useEffect(() => {
    if (!open) return;
    const controller = new AbortController();
    setExpanded(false);
    setQuery("");
    setLoading(true);
    setFailed("");
    setRows([]);
    setSelected("");
    api.serverVersions(channel, controller.signal).then(result => {
      if (controller.signal.aborted || result.channel !== channel) return;
      const values = result.versions;
      setPlatform(result.platform);
      setRows(values);
      const available = values.filter(row => !row.reason);
      const preferred = available.find(row => row.version === initial) ?? available[0];
      setSelected(canInstall && preferred ? preferred.version : "");
    }).catch(error => {
      if (!controller.signal.aborted) setFailed(apiErrorText(common, error));
    }).finally(() => {
      if (!controller.signal.aborted) setLoading(false);
    });
    return () => controller.abort();
  }, [open, channel, revision, initial, canInstall, common]);

  const search = query.trim().toLowerCase();
  const visibleRows = expanded
    ? rows.filter(row => row.version.toLowerCase().includes(search) || row.commit?.toLowerCase().includes(search))
    : rows.filter((row, index) => index < recentLimit || row.version === selected);
  const choice = visibleRows.find(row => row.version === selected && !row.reason);
  const source = t(channel === "release" ? "updates.channelRelease" : "updates.channelNightly");
  const refresh = () => {
    onRefresh();
    setRevision(value => value + 1);
  };

  return <SideDrawer open={open} onClose={onClose} title={t("versionPicker.title")}
    subtitle={t("versionPicker.source", { channel: source })}
    footer={<>
      <Button variant="secondary" onClick={onClose}>{t("common.cancel")}</Button>
      <Button disabled={!choice || loading || Boolean(failed) || !canInstall} onClick={() => choice && onChoose(choice)}>
        {t("versionPicker.upgradeTo", { version: choice?.version || t("versionPicker.selectedTarget") })}
      </Button>
    </>}>
    <div className="flex min-w-0 flex-col gap-4">
      <DetailList aria-label={t("updates.currentBuild")}>
        <DetailListItem>
          <DetailListLabel>{t("versionPicker.runningVersion")}</DetailListLabel>
          <DetailListValue>{server.current}</DetailListValue>
        </DetailListItem>
        <DetailListItem>
          <DetailListLabel>{t("versionPicker.runningChannel")}</DetailListLabel>
          <DetailListValue>{server.current_build?.build_channel || "—"}</DetailListValue>
        </DetailListItem>
      </DetailList>
      {failed && <Notice tone="warning" title={t("versionPicker.lookupFailed")}>{failed}</Notice>}
      {problem && <Notice tone="warning" title={problem} />}
      {!canInstall && <Notice tone="warning" title={blocker || t("versionPicker.cannotInstall")}>{t("versionPicker.useHost")}</Notice>}
      <section aria-label={t("versionPicker.available")}>
        <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
          <h3 className="m-0 text-body font-semibold">{t("versionPicker.available")}</h3>
          <div className="flex flex-wrap items-center gap-1">
            {rows.length > recentLimit && <Button variant="ghost" size="sm" aria-expanded={expanded}
              onClick={() => { setExpanded(!expanded); setQuery(""); }}>
              {expanded ? t("versionPicker.showRecent") : t("versionPicker.showAll", { count: rows.length })}
            </Button>}
            <Button variant="ghost" size="sm" disabled={loading} onClick={refresh}>{t("versionPicker.recheck")}</Button>
          </div>
        </div>
        {expanded && <Input type="search" size="sm" className="mb-2" value={query}
          aria-label={t("versionPicker.search")} placeholder={t("versionPicker.search")}
          onChange={event => setQuery(event.target.value)} />}
        {expanded && search && visibleRows.length === 0 && <p role="status" className="text-body text-fg-muted">{t("versionPicker.noMatch")}</p>}
        {loading && <p role="status" className="text-body text-fg-muted">{t("versionPicker.loading")}</p>}
        {!loading && !failed && rows.length === 0 && <Notice title={t("versionPicker.empty")} />}
        {visibleRows.length > 0 && <RadioGroup aria-label={t("versionPicker.available")} name="release-id" value={choice?.version ?? ""}
          onValueChange={setSelected} className="gap-2">
          {visibleRows.map(row => {
            const latest = row.latest ?? (row === rows[0]);
            const blocked = row.reason;
            const noteId = `gw-version-note-${row.version}`;
            return <label key={row.version} className={`gw-version${selected === row.version ? " is-selected" : ""}`}>
              <RadioGroupItem value={row.version} aria-label={row.version} aria-describedby={blocked || latest ? `${noteId}-meta ${noteId}` : `${noteId}-meta`} disabled={Boolean(blocked) || !canInstall} />
              <span className="gw-version-main"><strong>{row.version}</strong><small id={`${noteId}-meta`}>{formatTime(row.built_at)} · {platform}</small>
                {search && row.commit && !row.version.toLowerCase().includes(search) && <small>{row.commit}</small>}
              </span>
              <span className="gw-version-note inline-flex flex-wrap gap-1" id={noteId}>
                {latest && <Badge size="sm" status="info">{t("versionPicker.latest")}</Badge>}
                {blocked && <Badge size="sm" status="neutral">{t(`versionPicker.reasons.${blocked}`, { platform })}</Badge>}
              </span>
            </label>;
          })}
        </RadioGroup>}
      </section>
      {rows.some(row => row.reason === "backup") && <details>
        <summary>{t("versionPicker.recoveryTitle")}</summary>
        <p>{t("versionPicker.recoveryHelp")}</p>
      </details>}
    </div>
  </SideDrawer>;
}
