import { WarningOutlined } from "@ant-design/icons";
import { useState, useSyncExternalStore, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { toast, Toaster } from "../../components/ui/toast";
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogFooter } from "../../components/ui/dialog";
import { Button } from "../../components/ui/button";

type MessageContent = ReactNode | { content: ReactNode; duration?: number; key?: string | number };
function notify(status: "success" | "error" | "info" | "neutral" | "loading" | "warning", input: MessageContent, duration?: number) {
  const options = input && typeof input === "object" && "content" in input ? input : { content: input, duration };
  const id = toast[status === "warning" ? "neutral" : status](options.content, { icon: status === "warning" ? <WarningOutlined className="text-fg-warning" /> : undefined, duration: options.duration == null ? (status === "loading" ? 0 : 3000) : options.duration * 1000, id: "key" in options ? options.key : undefined });
  return () => toast.dismiss(id);
}
/** Temporary message call bridge. Migrate callers as their page moves to Zeron. */
export const message = {
  success: (content: MessageContent, duration?: number) => notify("success", content, duration),
  error: (content: MessageContent, duration?: number) => notify("error", content, duration),
  info: (content: MessageContent, duration?: number) => notify("info", content, duration),
  warning: (content: MessageContent, duration?: number) => notify("warning", content, duration),
  loading: (content: MessageContent, duration?: number) => notify("loading", content, duration),
  destroy: (key?: string | number) => toast.dismiss(key),
};
export interface ConfirmOptions {
  title: ReactNode; content?: ReactNode; okText?: string; cancelText?: string;
  okButtonProps?: { danger?: boolean; disabled?: boolean };
  onOk?: () => void | Promise<unknown>; onCancel?: () => void;
}
type Confirmation = ConfirmOptions & { id: number };
let queue: Confirmation[] = [];
let seed = 0;
const listeners = new Set<() => void>();
const emit = () => listeners.forEach((listener) => listener());
const remove = (id: number) => { queue = queue.filter((item) => item.id !== id); emit(); };
const subscribe = (listener: () => void) => { listeners.add(listener); return () => { listeners.delete(listener); }; };
/** Explicitly scoped subset of Modal.confirm, with async retry and controlled dismissal. */
export function confirm(options: ConfirmOptions) {
  const id = ++seed;
  queue = [...queue, { ...options, id }]; emit();
  return {
    destroy: () => remove(id),
    update: (patch: Partial<ConfirmOptions>) => { queue = queue.map((item) => item.id === id ? { ...item, ...patch } : item); emit(); },
  };
}
function ConfirmationDialog({ item }: { item: Confirmation }) {
  const { t } = useTranslation("common");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const cancel = () => { if (!pending) { item.onCancel?.(); remove(item.id); } };
  const accept = async () => {
    if (pending) return;
    setPending(true); setError(null);
    try { await item.onOk?.(); remove(item.id); }
    catch (cause) { setError(cause instanceof Error ? cause.message : t("feedback.requestFailed")); }
    finally { setPending(false); }
  };
  return <Dialog open onOpenChange={(open) => { if (!open) cancel(); }} disablePointerDismissal>
    <DialogContent closeButtonLabel={t("actions.close")} closeButtonDisabled={pending}>
      <DialogHeader><DialogTitle>{item.title}</DialogTitle></DialogHeader>
      {item.content}
      {error && <p role="alert" className="text-fg-danger">{error}</p>}
      <DialogFooter>
        <Button variant="ghost" disabled={pending} onClick={cancel}>{item.cancelText ?? t("actions.cancel")}</Button>
        <Button variant={item.okButtonProps?.danger ? "destructive" : "primary"} disabled={item.okButtonProps?.disabled} loading={pending} onClick={() => void accept()}>{item.okText ?? t("actions.confirm")}</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>;
}
export function FeedbackHost() {
  const { t } = useTranslation("common");
  const items = useSyncExternalStore(subscribe, () => queue, () => queue);
  return <><Toaster position="top-center" closeLabel={t("actions.close")} aria-label={t("notices.title")} />
    {items[0] && <ConfirmationDialog key={items[0].id} item={items[0]} />}
  </>;
}
