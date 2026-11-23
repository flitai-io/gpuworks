import { useCallback, useState, type RefObject, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "../../components/ui/button";
import { PopoverPortalContainer } from "../../components/ui/popover";
import { ConfirmPopover } from "../../ui-kit/ConfirmPopover";

type Props = {
  triggerRef?: RefObject<HTMLButtonElement | null>;
  onSave: () => void | Promise<unknown>;
  loading?: boolean;
  disabled?: boolean;
  label?: string;
  title?: string;
  description?: ReactNode;
  confirmText?: string;
  destructive?: boolean;
  secondary?: boolean;
};

export default function SettingsSaveButton({ triggerRef, onSave, loading, disabled, label, title, description, confirmText, destructive, secondary }: Props) {
  const { t } = useTranslation("settings");
  const [container, setContainer] = useState<HTMLElement | null>(null);
  // Keep the confirmation inside the active dialog's focus and stacking context.
  const anchor = useCallback((node: HTMLButtonElement | null) => {
    if (triggerRef) triggerRef.current = node;
    setContainer(node?.closest<HTMLElement>('.ant-modal-content, .ant-drawer-content, [data-slot="dialog-content"]') ?? null);
  }, [triggerRef]);
  return <PopoverPortalContainer value={container}>
    <ConfirmPopover
      trigger={<Button ref={anchor} type="button" size="sm" variant={secondary ? "secondary" : "primary"} loading={loading} disabled={disabled || loading}>{label ?? t("common.save")}</Button>}
      title={title ?? t("common.confirmSave")}
      onConfirm={() => { if (!disabled && !loading) return onSave(); }}
      confirmText={confirmText}
      destructive={destructive}
    >{description}</ConfirmPopover>
  </PopoverPortalContainer>;
}
