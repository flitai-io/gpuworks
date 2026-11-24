import { useTranslation } from "react-i18next";

import { Button } from "./ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "./ui/dialog";

/** Asks before the channel's system image catalog replaces one uploaded by hand: that deletes the versions only the upload has. */
export default function ReplaceUploadedCatalogDialog({ versions, onCancel, onConfirm }: {
  /** The uploaded catalog in force and the channel's; null keeps the dialog closed. */
  versions: { current?: string; latest?: string } | null;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  return (
    <Dialog open={Boolean(versions)} onOpenChange={open => { if (!open) onCancel(); }}>
      <DialogContent closeButtonLabel={tCommon("actions.close")}>
        <DialogHeader><DialogTitle>{t("updates.useChannelTitle")}</DialogTitle></DialogHeader>
        <p>{t("updates.useChannelConfirm", { current: versions?.current, latest: versions?.latest })}</p>
        <DialogFooter>
          <Button variant="secondary" onClick={onCancel}>{tCommon("actions.cancel")}</Button>
          <Button variant="destructive" onClick={onConfirm}>{t("updates.useChannel")}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
