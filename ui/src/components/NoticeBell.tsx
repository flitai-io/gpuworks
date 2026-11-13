import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Badge, type BadgeStatus } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Empty, EmptyDescription, EmptyHeader } from "@/components/ui/empty";
import { Popover, PopoverContent, PopoverTitle, PopoverTrigger } from "@/components/ui/popover";
import { Tooltip } from "@/components/ui/tooltip";
import { api, type NoticeItem, type NoticeList } from "../api";
import { formatTime } from "../i18n/datetime";
import { ConsoleIcon } from "../ui-kit/consoleIcons";

const BellIcon = ConsoleIcon.notifications;

// How often an open console asks for notices again. The server fetches them
// from the distribution channel with its update check, at most hourly.
const refreshMs = 30 * 60_000;

const severityStatus: Record<NoticeItem["severity"], BadgeStatus> = { critical: "danger", important: "warning", info: "info" };

// localized picks the publisher's text for the console's language, falling
// back to English, which every notice carries.
function localized(texts: Record<string, string> | undefined, language: string): string {
  if (!texts) return "";
  const base = language.split("-")[0];
  return texts[language] ?? texts[base] ?? texts.en ?? "";
}

// NoticeBell lists security advisories, releases and events the publisher
// addressed to this installation. Nothing pops up: a badge counts what the
// administrator has not read.
export default function NoticeBell() {
  const { t, i18n } = useTranslation();
  const [list, setList] = useState<NoticeList | null>(null);
  const [failed, setFailed] = useState(false);
  const [open, setOpen] = useState(false);

  const load = useCallback(async () => {
    try {
      setList(await api.notices());
      setFailed(false);
    } catch {
      setFailed(true);
    }
  }, []);

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), refreshMs);
    return () => window.clearInterval(timer);
  }, [load]);

  const markRead = async (ids: string[]) => {
    if (ids.length === 0) return;
    try {
      await api.readNotices(ids);
    } finally {
      await load();
    }
  };

  const unread = list?.unread ?? 0;
  const notices = list?.notices ?? [];
  const bellLabel = unread > 0 ? `${t("notices.title")} (${t("notices.unread", { count: unread })})` : t("notices.title");

  return (
    <Popover open={open} onOpenChange={(next) => { setOpen(next); if (next) void load(); }}>
      <Tooltip content={unread > 0 ? t("notices.unread", { count: unread }) : t("notices.title")}>
        <PopoverTrigger
          render={(
            <Button variant="ghost" size="md" iconOnly aria-label={bellLabel} className="relative">
              <BellIcon />
              {unread > 0 && (
                <span aria-hidden="true" className="absolute right-0.5 top-0.5 flex h-4 min-w-4 items-center justify-center rounded-full bg-danger-strong px-1 text-[11px] leading-none text-fg-on-danger-strong">
                  {unread}
                </span>
              )}
            </Button>
          )}
        />
      </Tooltip>
      <PopoverContent align="end" className="w-[380px] max-w-[calc(100vw-32px)] p-0">
        <div className="flex items-center justify-between gap-2 border-b border-border-subtle px-4 py-2.5">
          <PopoverTitle>{t("notices.title")}</PopoverTitle>
          {unread > 0 && (
            <Button variant="ghost" size="xs" className="text-fg-brand" onClick={() => void markRead(notices.filter((n) => !n.read).map((n) => n.id))}>
              {t("notices.markAllRead")}
            </Button>
          )}
        </div>
        <div className="max-h-[420px] overflow-auto">
          {failed && !list && <p className="px-4 py-3 text-body text-fg-muted">{t("notices.loadFailed")}</p>}
          {list && notices.length === 0 && (
            <Empty reason="no-data" scope="inline" density="compact">
              <EmptyHeader><EmptyDescription>{t("notices.empty")}</EmptyDescription></EmptyHeader>
            </Empty>
          )}
          {notices.length > 0 && (
            <ul className="list-none divide-y divide-border-subtle">
              {notices.map((notice) => {
                const title = localized(notice.title, i18n.language);
                const body = localized(notice.body, i18n.language);
                return (
                  <li key={notice.id} data-testid={`notice-${notice.id}`} className={`flex items-start gap-3 px-4 py-3 ${notice.read ? "opacity-65" : ""}`}>
                    <div className="flex min-w-0 flex-1 flex-col gap-1">
                      <div className="flex flex-wrap items-center gap-1">
                        <Badge status={severityStatus[notice.severity]} size="sm">{t(`notices.severity.${notice.severity}`)}</Badge>
                        <Badge size="sm">{t(`notices.kind.${notice.kind}`)}</Badge>
                        <span className="text-label text-fg-subtle">{formatTime(notice.published_at)}</span>
                      </div>
                      <span className={`text-body text-fg-default ${notice.read ? "" : "font-medium"}`}>{title}</span>
                      {body && <p className="whitespace-pre-wrap text-body text-fg-muted">{body}</p>}
                      {notice.url && (
                        <a className="text-body text-fg-brand hover:underline" href={notice.url} target="_blank" rel="noopener noreferrer">{t("notices.readMore")}</a>
                      )}
                    </div>
                    {!notice.read && (
                      <Button variant="ghost" size="xs" className="shrink-0 text-fg-brand" onClick={() => void markRead([notice.id])}>{t("notices.markRead")}</Button>
                    )}
                  </li>
                );
              })}
            </ul>
          )}
        </div>
      </PopoverContent>
    </Popover>
  );
}
