import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { Tooltip } from "@/components/ui/tooltip";
import { fromAntd } from "../ui-kit/antdIcon";
import { CloudDownloadOutlined } from "@ant-design/icons";
import { useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, type UpdateStatus } from "../api";

const DownloadIcon = fromAntd(CloudDownloadOutlined);

// How often one browser asks the server whether updates exist. The server
// itself reuses its last check of the distribution channel for an hour, so this
// only keeps page reloads from repeating the request.
const recheckMs = 60 * 60_000;
const storageKey = "gpuworks.updates.checked";

export function hasUpdate(status: UpdateStatus): boolean {
  return status.available && (
    status.server.update_available ||
    status.image_catalog.update_available ||
    (status.template_catalog.update_available && status.template_catalog.compatible)
  );
}

function recentlyChecked(): boolean | null {
  try {
    const raw = window.localStorage.getItem(storageKey);
    if (!raw) return null;
    const saved = JSON.parse(raw) as { at: number; update: boolean };
    return Date.now() - saved.at < recheckMs ? saved.update : null;
  } catch {
    return null;
  }
}

function remember(update: boolean) {
  try {
    window.localStorage.setItem(storageKey, JSON.stringify({ at: Date.now(), update }));
  } catch {
    // Storage unavailable: the next page load checks again.
  }
}

const updatedEvent = "gpuworks:update-status";

// publishUpdateStatus lets the header follow a check made elsewhere (the
// settings card), so the notice goes away once everything is applied.
export function publishUpdateStatus(status: UpdateStatus) {
  const found = hasUpdate(status);
  remember(found);
  window.dispatchEvent(new CustomEvent(updatedEvent, { detail: found }));
}

// UpdateNotice checks for updates when the console opens and, when there is
// one, shows a button in the header that leads to Version and updates.
export default function UpdateNotice() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [update, setUpdate] = useState(false);

  useEffect(() => {
    const follow = (event: Event) => setUpdate(Boolean((event as CustomEvent<boolean>).detail));
    window.addEventListener(updatedEvent, follow);
    return () => window.removeEventListener(updatedEvent, follow);
  }, []);

  useEffect(() => {
    const cached = recentlyChecked();
    if (cached !== null) {
      setUpdate(cached);
      return;
    }
    let cancelled = false;
    api.updates()
      .then((status) => {
        const found = hasUpdate(status);
        remember(found);
        if (!cancelled) setUpdate(found);
      })
      .catch(() => {
        // Not an administrator, or the server could not check: say nothing.
      });
    return () => { cancelled = true; };
  }, []);

  if (!update) return null;
  return (
    <Tooltip content={t("header.updateAvailableHint")}>
      {/* A pale info capsule: the only header item that is a notice, not a tool (ui-guidelines 9.1b). */}
      <Button variant="ghost" size="sm" leadingIcon={DownloadIcon} className="app-header-update rounded-full bg-info-surface text-fg-info hover:text-fg-info" onClick={() => navigate("/settings#updates")}>
        {t("header.updateAvailable")}
      </Button>
    </Tooltip>
  );
}
