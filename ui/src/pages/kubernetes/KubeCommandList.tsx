import { useTranslation } from "react-i18next";
import CopyButton from "../../components/CopyButton";

/** kubectl commands to run in the cluster, in order, each with a button that copies it and one that copies them all. */
export default function KubeCommandList({ commands }: { commands: string[] }) {
  const { t } = useTranslation("kubernetes");
  return (
    <div className="flex min-w-0 flex-col gap-2">
      <ol className="m-0 flex list-none flex-col gap-2 p-0">
        {commands.map((command, index) => (
          <li key={`${index}-${command}`} className="flex items-start gap-1">
            <span className="mt-2 w-5 shrink-0 text-right text-label text-fg-muted">{index + 1}.</span>
            <pre aria-label={t("commands.command", { n: index + 1 })} tabIndex={0} className="code-block min-w-0 flex-1 whitespace-pre-wrap break-all p-3 text-label">{command}</pre>
            <CopyButton size="small" label={t("commands.copy", { n: index + 1 })} text={command} />
          </li>
        ))}
      </ol>
      {commands.length > 1 && (
        <div>
          <CopyButton type="default" size="small" label={t("commands.copyAll")} text={commands.join("\n")}>{t("commands.copyAll")}</CopyButton>
        </div>
      )}
    </div>
  );
}
