import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "../../components/ui/alert";
import { Button } from "../../components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../components/ui/dialog";
import { Input } from "../../components/ui/input";

type Props = {
  imageRef: string;
  nodeLabel: string;
  onCancel: () => void;
  onStart: (path: string) => Promise<void>;
};

export default function ImageArchivePathDialog({ imageRef, nodeLabel, onCancel, onStart }: Props) {
  const { t } = useTranslation("images");
  const { t: common } = useTranslation("common");
  const [path, setPath] = useState("");
  const [starting, setStarting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const submit = async () => {
    if (!path.trim() || starting) return;
    setStarting(true);
    setError(null);
    try {
      await onStart(path.trim());
    } catch (e) {
      setError(e instanceof Error ? e.message : t("action.pullFailed"));
    } finally {
      setStarting(false);
    }
  };
  return (
    <Dialog open onOpenChange={(open) => { if (!open && !starting) onCancel(); }}>
      <DialogContent size="lg" closeButtonLabel={common("actions.close")} closeButtonDisabled={starting}>
        <DialogHeader>
          <DialogTitle>{t("nodeTable.localDialog.title", { node: nodeLabel })}</DialogTitle>
          <DialogDescription>{t("nodeTable.localDialog.description", { ref: imageRef })}</DialogDescription>
        </DialogHeader>
        <form onSubmit={(event) => { event.preventDefault(); void submit(); }}>
          <div className="space-y-2">
            <label htmlFor="image-archive-path" className="text-body">{t("nodeTable.localDialog.path")}</label>
            <Input
              id="image-archive-path"
              value={path}
              disabled={starting}
              aria-describedby="image-archive-path-help"
              placeholder="/data/images/runner.tar"
              onChange={(event) => { setPath(event.target.value); setError(null); }}
            />
            <p id="image-archive-path-help" className="text-label text-fg-muted">{t("nodeTable.localDialog.help")}</p>
            {error ? <Alert status="danger"><AlertDescription>{error}</AlertDescription></Alert> : null}
          </div>
          <DialogFooter>
            <Button type="button" disabled={starting} onClick={onCancel}>{t("common.cancel")}</Button>
            <Button type="submit" variant="primary" disabled={starting || !path.trim()} aria-busy={starting}>
              {t("nodeTable.localDialog.start")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
