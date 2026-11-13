import { useState, type ReactElement, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Popover, PopoverContent, PopoverFooter, PopoverTitle, PopoverTrigger } from "../components/ui/popover";
import { Button } from "../components/ui/button";
export function ConfirmPopover({ trigger, title, children, onConfirm, confirmText, cancelText, destructive = false }: {
  trigger: ReactElement; title: ReactNode; children?: ReactNode; onConfirm: () => void | Promise<unknown>;
  confirmText?: string; cancelText?: string; destructive?: boolean;
}) {
  const { t } = useTranslation("common");
  const [open, setOpen] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const confirm = async () => {
    if (pending) return;
    setPending(true); setError(null);
    try { await onConfirm(); setOpen(false); }
    catch (cause) { setError(cause instanceof Error ? cause.message : t("feedback.requestFailed")); }
    finally { setPending(false); }
  };
  return <Popover open={open} onOpenChange={(next) => { if (!pending) { setOpen(next); setError(null); } }}>
    <PopoverTrigger render={trigger} />
    <PopoverContent><PopoverTitle>{title}</PopoverTitle>{children}
      {error && <p role="alert" className="text-fg-danger">{error}</p>}
      <PopoverFooter>
        <Button variant="ghost" disabled={pending} onClick={() => setOpen(false)}>{cancelText ?? t("actions.cancel")}</Button>
        <Button variant={destructive ? "destructive" : "primary"} loading={pending} onClick={() => void confirm()}>{confirmText ?? t("actions.confirm")}</Button>
      </PopoverFooter>
    </PopoverContent>
  </Popover>;
}
