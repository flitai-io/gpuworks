import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type KubePrecheck, type KubePrecheckRequest } from "../../api";
import { apiErrorText } from "../../apiError";
import { saveBlob } from "../../apiFiles";
import CopyButton from "../../components/CopyButton";
import { Button } from "../../components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../components/ui/dialog";
import { HelpTip } from "../../ui-kit/HelpTip";
import { Notice } from "../../ui-kit/Notice";
import { Spinner } from "../../ui-kit/Spinner";
import KubeCheckCodes from "./KubeCheckCodes";

/**
 * The pre-check for the cluster form's current fields: a script to run where
 * kubectl reaches the cluster, before connecting it. It starts the agent image
 * on every node the agents would run on and prints, per node, whether the
 * image was pulled and the server reached. Running it is a recommendation,
 * never a condition for connecting. Mount it while it is open.
 */
export default function KubePrecheckModal({ request, codes, onClose }: { request: KubePrecheckRequest; codes: string[]; onClose: () => void }) {
  const { t } = useTranslation("kubernetes");
  const { t: tCommon } = useTranslation("common");
  const [precheck, setPrecheck] = useState<KubePrecheck | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let alive = true;
    api.kubePrecheck(request)
      .then((out) => { if (alive) setPrecheck(out); })
      .catch((e: unknown) => { if (alive) setError(apiErrorText(tCommon, e)); });
    return () => { alive = false; };
  }, [request, tCommon]);
  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose(); }}>
      <DialogContent closeButtonLabel={tCommon("actions.close")} style={{ width: "min(760px, calc(100vw - 32px))", maxWidth: "calc(100vw - 32px)" }} className="flex max-h-[calc(100dvh-32px)] flex-col gap-4">
        <DialogHeader><DialogTitle>{t("precheck.title")}</DialogTitle></DialogHeader>
        <div className="flex min-h-0 min-w-0 flex-col gap-3 overflow-auto">
          {error !== null && <Notice tone="danger" title={t("precheck.failed")}>{error}</Notice>}
          {error === null && precheck === null && <Spinner label={t("precheck.loading")} />}
          {precheck !== null && (
            <>
              <span>{t("precheck.intro")}</span>
              <dl className="m-0 grid grid-cols-[max-content_minmax(0,1fr)] gap-x-3 gap-y-1">
                <dt className="text-fg-muted">{t("precheck.image")}</dt><dd className="m-0 break-all"><code>{precheck.image}</code></dd>
                <dt className="text-fg-muted">{t("precheck.server")}</dt><dd className="m-0 break-all"><code>{precheck.server}</code></dd>
              </dl>
              <strong className="inline-flex items-center gap-1 font-semibold text-fg-default">{t("precheck.run")}<HelpTip content={t("precheck.runHelp")} /></strong>
              <pre aria-label={t("precheck.run")} tabIndex={0} className="code-block m-0 whitespace-pre-wrap break-all p-3">{`bash ${precheck.file_name}`}</pre>
              <strong className="font-semibold text-fg-default">{t("check.codesTitle")}</strong>
              <KubeCheckCodes codes={codes} />
              <details>
                <summary className="cursor-pointer text-fg-muted">{t("precheck.showScript")}</summary>
                <pre aria-label={t("precheck.script")} tabIndex={0} className="code-block mt-2 max-h-[40vh] overflow-auto p-3">{precheck.script}</pre>
              </details>
            </>
          )}
        </div>
        <DialogFooter>
          {precheck !== null && <CopyButton type="default" label={t("precheck.copy")} text={precheck.script}>{t("precheck.copy")}</CopyButton>}
          {precheck !== null && (
            <Button variant="neutral" onClick={() => saveBlob(new Blob([precheck.script], { type: "text/x-shellscript" }), precheck.file_name)}>{t("precheck.download")}</Button>
          )}
          <Button variant="primary" onClick={onClose}>{t("precheck.close")}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
