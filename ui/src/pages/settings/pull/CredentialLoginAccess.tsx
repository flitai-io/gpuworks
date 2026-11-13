import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { Credential } from "../../../api";
import { OutboundHostAccess, useOutboundHostAccess } from "../../../components/OutboundHostAccess";
import { Button } from "../../../components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../../components/ui/dialog";
import { credentialLoginHost, isSiteToken } from "./CredentialDrawer";

/** Test the stored login without saving or replacing any of its fields. */
export default function CredentialLoginAccess({ credential, onClose, onTest }: {
  credential: Credential;
  onClose: () => void;
  onTest: () => Promise<void>;
}) {
  const { t } = useTranslation("settings");
  const access = useOutboundHostAccess([credentialLoginHost(credential.host, isSiteToken(credential))]);
  const [busy, setBusy] = useState(false);
  const ready = access.policy && !access.pending && !access.error && (
    !access.policy.restrict_private_hosts || access.checks.length === access.hosts.length && access.checks.every((item) =>
      item.status === "allowed" || item.status === "needs_allow" && access.policy?.editable && access.selected.includes(item.host))
  );
  const run = async () => {
    if (!ready || busy) return;
    setBusy(true);
    try {
      await access.authorize();
      await onTest();
      onClose();
    } catch {
      // The shared access control presents authorization errors and retry.
    } finally {
      setBusy(false);
    }
  };

  return <Dialog open onOpenChange={(open) => { if (!open && !busy) onClose(); }}>
    <DialogContent closeButtonDisabled={busy}>
      <DialogHeader>
        <DialogTitle>{t("pull.credentials.testFor", { host: credential.host })}</DialogTitle>
        <DialogDescription>{t("pull.credentials.host")}: {credential.host}</DialogDescription>
      </DialogHeader>
      <OutboundHostAccess access={access} />
      <DialogFooter>
        <Button variant="secondary" disabled={busy} onClick={onClose}>{t("pull.action.cancel")}</Button>
        <Button disabled={busy || !ready} onClick={() => void run()}>{t("pull.credentials.test")}</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>;
}
