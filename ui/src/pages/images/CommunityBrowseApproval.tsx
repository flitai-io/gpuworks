import { useState } from "react";
import { useTranslation } from "react-i18next";
import { OutboundHostAccess, useOutboundHostAccess } from "../../components/OutboundHostAccess";
import { Button } from "../../components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../components/ui/dialog";

/** Consent is scoped to this clicked repository, independently of the manual input. */
export default function CommunityBrowseApproval({ repo, address, onBrowse, onClose }: {
  repo: string;
  address: string;
  onBrowse: () => Promise<boolean>;
  onClose: () => void;
}) {
  const { t } = useTranslation("images");
  const { t: common } = useTranslation("common");
  const access = useOutboundHostAccess([address], Boolean(address));
  const [busy, setBusy] = useState(false);
  const submit = async () => {
    setBusy(true);
    try {
      await access.authorize();
      if (await onBrowse()) onClose();
    } catch {
      // The shared control keeps authorization errors next to the selection.
    } finally { setBusy(false); }
  };
  return <Dialog open onOpenChange={(open) => { if (!open && !busy) onClose(); }}>
    <DialogContent closeButtonLabel={common("actions.close")} closeButtonDisabled={busy}>
      <DialogHeader>
        <DialogTitle>{t("import.community.title")}</DialogTitle>
        <DialogDescription>{repo}</DialogDescription>
      </DialogHeader>
      {address ? <OutboundHostAccess access={access} /> : <p className="text-sm text-fg-muted">
        {t("import.community.accessUnknown")}{" "}
        <a className="underline" href="/settings#outbound-policy">{common("outboundAccess.settings")}</a>
      </p>}
      <DialogFooter>
        <Button disabled={busy} onClick={onClose}>{common("actions.cancel")}</Button>
        <Button variant="primary" disabled={busy || access.pending} onClick={() => void submit()}>{t("import.community.browse")}</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>;
}
