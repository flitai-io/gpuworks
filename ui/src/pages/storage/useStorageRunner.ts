import { useState } from "react";
import { message } from "antd";
import { useTranslation } from "react-i18next";
import { storageErrorText } from "../../storageDisplay";

/**
 * Runs one storage action at a time: remembers which one is busy, turns a
 * failure into text for the page to show, and tells the page to reload on success.
 */
export function useStorageRunner(onChanged: () => void) {
  const { t } = useTranslation("storage");
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  /** Resolves true when the action worked. `reload` is false for an action after which the page has nothing to reload (a delete). */
  const run = async (name: string, action: () => Promise<unknown>, success?: string, reload = true): Promise<boolean> => {
    setBusy(name);
    setError(null);
    try {
      await action();
      if (success) message.success(success);
      if (reload) onChanged();
      return true;
    } catch (e) {
      setError(storageErrorText(t, e));
      return false;
    } finally {
      setBusy(null);
    }
  };
  return { busy, error, clearError: () => setError(null), run, reload: onChanged };
}
