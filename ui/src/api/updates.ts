import type { ServerBuildInfo } from "../serverBuildInfo";
import type { ProductMessage } from "../apiError";

/** One distribution source's answer to an update check (ADR 0040). */
export interface UpdateSourceStatus {
  url: string;
  ok: boolean;
  latency_ms: number;
  error?: string;
}

/** An official catalog in force next to the published one. */
export interface CatalogUpdate {
  current: string;
  latest?: string;
  update_available: boolean;
  /** The catalog in force was uploaded by hand (image catalog only): the channel's is then not its update. */
  uploaded?: boolean;
  /** True when this server can read the catalog; check_error explains a failed check. */
  compatible: boolean;
  /** Present when the catalog could not be checked; other updates remain usable. */
  check_error?: ProductMessage;
}

/** GET /api/v1/updates: what the distribution channel offers next to what this server runs. */
export interface UpdateStatus {
  /** nightly (滚动预览版) or release (正式发布版). */
  channel: string;
  /** The console can switch the channel (the server keeps it in server.yaml). */
  channel_editable: boolean;
  /**
   * Whether the server checks the channel by itself and tells it the fleet's
   * size and makeup (node count, GPU models, agent versions) while doing so.
   * False: it asks only on "check for updates", and says nothing then.
   */
  auto_check: boolean;
  checked_at: string;
  /** False when no source served a valid index; sources say why. */
  available: boolean;
  // Why no source served the channel's index when available is false:
  // the channel is reachable but empty, a source served an index that did
  // not verify, or none could be reached.
  unavailable_reason?: "unpublished" | "invalid" | "unreachable";
  source?: string;
  sources: UpdateSourceStatus[];
  server: {
    current_build?: ServerBuildInfo;
    current: string;
    latest?: string;
    built_at?: string;
    update_available: boolean;
    installable: boolean;
    /** True when the console can upgrade this server in place. */
    self_upgrade: boolean;
    /** Why it cannot: no_service_manager, not_installed, not_installed_binary, cluster_member or not_writable. */
    self_upgrade_blocker?: string;
    /** The installation directory the server runs from, when it was installed. */
    install_dir?: string;
  };
  image_catalog: CatalogUpdate;
  template_catalog: CatalogUpdate;
}

/** GET /api/v1/usage-report: the user experience improvement program. */
export interface UsageReportStatus {
  /** usage_report in server.yaml; absent means on. */
  enabled: boolean;
  /** The console can switch it (the server keeps it in server.yaml). */
  editable: boolean;
  /** The UTC day (YYYY-MM-DD) of the last report delivered. */
  last_day?: string;
  /** The latest delivery attempt since the server started. */
  last_attempt?: { at: string; day: string; ok: boolean; error?: string };
  next_due_at?: string;
}

/** One of the publisher's messages for the header's notice bell, with its own text per locale. */
export interface NoticeItem {
  id: string;
  severity: "critical" | "important" | "info";
  kind: "security" | "release" | "event";
  title?: Record<string, string>;
  body?: Record<string, string>;
  url?: string;
  published_at: string;
  read: boolean;
}

/** GET /api/v1/notices. */
export interface NoticeList {
  notices: NoticeItem[];
  unread: number;
}

/** A console upgrade that has swapped the binary and is restarting into `to`. */
export interface ServerVersionChoice {
  latest?: boolean;
  version: string;
  commit?: string;
  built_at: string;
  sha256?: string;
  reason?: "installed" | "arch" | "older" | "backup";
}

export interface ServerUpgradeStarted {
  from: string;
  to: string;
  backup: string;
}
