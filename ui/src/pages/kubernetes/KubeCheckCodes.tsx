import { useTranslation } from "react-i18next";
import { HelpTip } from "../../ui-kit/HelpTip";

/**
 * What each code a check script prints means, with what to do about it behind
 * its HelpTip. The
 * codes, and their order, are the server's (GET /kube/defaults check_codes).
 */
export default function KubeCheckCodes({ codes }: { codes: string[] }) {
  const { t } = useTranslation("kubernetes");
  return (
    <dl aria-label={t("check.codesTitle")} className="m-0 grid grid-cols-[max-content_minmax(0,1fr)] gap-x-3 gap-y-2">
      {codes.map((code) => (
        <div key={code} className="contents">
          <dt><code className="text-label">{code}</code></dt>
          <dd className="m-0 inline-flex items-start gap-1 text-fg-muted">
            {t(`check.codes.${code}.text`, { defaultValue: code })}
            <HelpTip label={t("check.fixLabel", { code })} content={t(`check.codes.${code}.fixHelp`, { defaultValue: "" })} />
          </dd>
        </div>
      ))}
    </dl>
  );
}
