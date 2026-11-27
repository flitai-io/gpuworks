import { useTranslation } from "react-i18next";
import CopyButton from "../../components/CopyButton";

/** A command to run in the cluster, with a button that copies it; by default the apply command. */
export default function ApplyCommand({ command, title, hint }: { command: string; title?: string; hint?: string }) {
  const { t } = useTranslation("kubernetes");
  const label = title ?? t("created.command");
  return (
    <div className="flex min-w-0 flex-col gap-2">
      <div className="flex items-center justify-between gap-2">
        <strong className="font-semibold text-fg-default">{label}</strong>
        <CopyButton type="default" size="small" label={title ? t("check.copyCommand", { title }) : t("created.copyCommand")} text={command}>{title ? t("check.copy") : t("created.copyCommand")}</CopyButton>
      </div>
      {hint && <span className="text-fg-muted">{hint}</span>}
      <pre aria-label={label} tabIndex={0} className="code-block whitespace-pre-wrap break-all p-4">
        {command}
      </pre>
    </div>
  );
}
