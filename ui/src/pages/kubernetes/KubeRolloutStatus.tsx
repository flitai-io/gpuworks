import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type KubeRollout } from "../../api";
import { apiErrorText } from "../../apiError";
import { Button } from "../../components/ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../components/ui/table";
import { HelpTip } from "../../ui-kit/HelpTip";
import { Notice } from "../../ui-kit/Notice";
import { Spinner } from "../../ui-kit/Spinner";

/**
 * How the agents of an entry made from a cloud cluster stand, node by node,
 * as the server reads them from the cluster. While the server says the
 * answer may still change it asks again after the delay the server gives;
 * mounted only while it is shown.
 */
export default function KubeRolloutStatus({ uuid }: { uuid: string }) {
  const { t } = useTranslation("kubernetes");
  const { t: tCommon } = useTranslation("common");
  const [rollout, setRollout] = useState<KubeRollout | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    let alive = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const read = () => {
      api.kubeRollout(uuid)
        .then((r) => {
          if (!alive) return;
          setRollout(r);
          setError(null);
          if (!r.done && r.poll_after_seconds) timer = setTimeout(read, r.poll_after_seconds * 1000);
        })
        .catch((e: unknown) => { if (alive) setError(apiErrorText(tCommon, e)); });
    };
    read();
    return () => { alive = false; if (timer) clearTimeout(timer); };
  }, [uuid, attempt, tCommon]);

  return (
    <section aria-label={t("rollout.title")} className="flex min-w-0 flex-col gap-2">
      <strong className="font-semibold text-fg-default">{t("rollout.title")}</strong>
      {error !== null && (
        <Notice tone="danger" title={t("rollout.failed")}>
          <div className="flex flex-col items-start gap-2">
            <span>{error}</span>
            <Button variant="secondary" size="sm" onClick={() => { setError(null); setAttempt((n) => n + 1); }}>{t("rollout.retry")}</Button>
          </div>
        </Notice>
      )}
      {error === null && rollout === null && <Spinner label={t("rollout.reading")} />}
      {rollout !== null && (
        <>
          <Table variant="bordered" size="sm">
            <TableHeader>
              <TableRow>
                <TableHead style={{ width: 200 }}>{t("rollout.column.node")}</TableHead>
                <TableHead>{t("rollout.column.state")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rollout.rows.map((row, index) => (
                <TableRow key={`${row.pod ?? ""}/${row.code}/${index}`} index={index}>
                  <TableCell><code>{row.node || "—"}</code></TableCell>
                  <TableCell>
                    <div className="flex min-w-0 flex-col gap-1">
                      <span className="inline-flex items-center gap-1">
                        <code className="text-label">{row.code}</code>
                        <span>{t(`check.codes.${row.code}.text`, { defaultValue: row.code })}</span>
                        <HelpTip label={t("check.fixLabel", { code: row.code })} content={t(`check.codes.${row.code}.fixHelp`, { defaultValue: "" })} />
                      </span>
                      {row.detail && <span className="break-all text-label text-fg-muted">{row.detail}</span>}
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {!rollout.done && rollout.poll_after_seconds && <span className="text-fg-muted">{t("rollout.waiting", { seconds: rollout.poll_after_seconds })}</span>}
        </>
      )}
    </section>
  );
}
