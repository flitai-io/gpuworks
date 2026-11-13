import { useTranslation } from "react-i18next";
import CopyButton from "../../components/CopyButton";

/** A command to run in the cluster, with a button that copies it. */
export default function ApplyCommand({ command }: { command: string }) {
  const { t } = useTranslation("kubernetes");
  return (
    <div className="flex min-w-0 flex-col gap-2">
      <div className="flex items-center justify-between gap-2">
        <strong className="font-semibold text-fg-default">{t("created.command")}</strong>
        <CopyButton type="default" size="small" label={t("created.copyCommand")} text={command}>{t("created.copyCommand")}</CopyButton>
      </div>
      <pre aria-label={t("created.command")} tabIndex={0} className="code-block whitespace-pre-wrap break-all p-4">
        {command}
      </pre>
    </div>
  );
}
