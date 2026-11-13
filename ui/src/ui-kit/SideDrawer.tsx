import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Dialog, DialogClose, DialogContent, DialogHeader, DialogTitle, DialogFooter } from "../components/ui/dialog";
import { Button } from "../components/ui/button";
import { ConsoleIcon } from "./consoleIcons";

/** Product dimensions and scroll ownership; Zeron owns portal, motion and focus. */
export function SideDrawer({ open, onClose, title, subtitle, children, extra, footer, width = 720, className, busy = false }: {
  open: boolean; onClose: () => void; title: ReactNode; children?: ReactNode;
  /** A line under the title (identifiers, tags). */
  subtitle?: ReactNode;
  /** Actions on the title row, right-aligned beside the close button, at their own size. */
  extra?: ReactNode; footer?: ReactNode; width?: number | string; className?: string; busy?: boolean;
}) {
  const { t } = useTranslation("common");
  return <Dialog open={open} onOpenChange={(next) => { if (!next && !busy) onClose(); }}>
    <DialogContent placement="right" className={`flex flex-col overflow-hidden ${className ?? ""}`}
      style={{ width: typeof width === "number" ? `min(${width}px, calc(100vw - 32px))` : width, maxWidth: "calc(100vw - 32px)" }}
      // The close button sits in the header row below. Zeron's own one is a
      // full-width sticky row laid over the header, which swallowed clicks on
      // the header actions next to the title.
      showCloseButton={false}>
      {/* Title and subtitle on the left, then the actions and the close button on
          the same row, as antd's Drawer laid them out. DialogHeader stacks its
          children and stretches them, which turned a switch into a full-width bar. */}
      <DialogHeader>
        <div className="flex min-w-0 items-start gap-3">
          <div className="min-w-0 flex-1">
            <DialogTitle>{title}</DialogTitle>
            {subtitle != null && <div className="mt-1 min-w-0 text-label text-fg-muted">{subtitle}</div>}
          </div>
          {extra != null && <div className="flex shrink-0 flex-wrap items-center justify-end gap-2">{extra}</div>}
          <DialogClose aria-label={t("actions.close")} disabled={busy}
            render={<Button variant="ghost" iconOnly size="sm" className="shrink-0 -mr-2"><ConsoleIcon.close size={16} /></Button>} />
        </div>
      </DialogHeader>
      <div className="min-h-0 min-w-0 flex-1 overflow-auto detail-body">{children}</div>
      {footer && <DialogFooter>{footer}</DialogFooter>}
    </DialogContent>
  </Dialog>;
}
