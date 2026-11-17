import type { TFunction } from "i18next";

/**
 * A recovery failure as an operator reads it: the localized sentence for its
 * code, then the recorded text as evidence. An error recorded before codes
 * has only its text, shown as written.
 */
export function recoveryErrorText(t: TFunction, code: string | undefined, message: string | undefined): string {
  const evidence = message?.trim() ?? "";
  if (!code) return evidence;
  const sentence = t(`apiError.${code}`, { ns: "common", defaultValue: "" });
  if (!sentence) return evidence;
  return evidence ? `${sentence}\n${evidence}` : sentence;
}

/** Why a recovery round did nothing, localized from its code. */
export function recoverySkippedText(t: TFunction, code: string | undefined, reason: string | undefined): string {
  if (!code) return reason ?? "";
  return t(`recovery.skipped.${code}`, { defaultValue: reason ?? "" });
}
