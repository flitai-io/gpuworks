import { productMessageText, type APIErrorTranslator, type ProductMessage } from "../../apiError";

const reasons = new Set([
  "source_ready", "cache_fill_required", "shared_transfer_pending", "copy_in_progress",
  "not_on_node", "model_missing_in_storage", "storage_unavailable", "storage_not_on_node",
  "storage_not_found", "storage_disabled", "facts_unavailable", "storage_access_required",
  "peer_available", "no_acquisition_path", "source_timeout", "source_cancelled", "source_unavailable",
  "metadata_not_checked", "source_not_required", "not_mounted", "permission_denied",
  "model_record_ready", "model_transfer_pending", "storage_state_unknown", "storage_ready", "metadata_only",
  "source_dns_failed", "source_tls_failed", "source_connect_failed", "source_authentication",
  "storage_mounting", "storage_unresponsive", "storage_unsupported", "storage_not_writable",
  "unsupported_storage", "shared_path_invalid",
  "no_storage_path", "peer_transport_required",
]);
const storageReasons = new Set(["missing_client", "unreachable", "source_mismatch", "not_root", "path_missing", "not_cached"]);

export function diagnosticMessageText(t: APIErrorTranslator, tCommon: APIErrorTranslator, message: ProductMessage): string {
  const reason = String(message.params?.reason ?? "");
  if (reasons.has(reason)) return t(`diagnostics.reason.${reason}`, { ...message.params });
  if (storageReasons.has(reason) && message.code === "STORAGE_UNAVAILABLE_ON_NODE") return tCommon(`storageReason.${reason}`, { ...message.params });
  if (reason && message.message) return message.message;
  return productMessageText(tCommon, message);
}
