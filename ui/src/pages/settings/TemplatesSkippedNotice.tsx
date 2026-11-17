import { useTranslation } from "react-i18next";
import type { OfficialInferenceCatalogSkipped } from "../../api";
import { Notice } from "../../ui-kit/Notice";

const shown = 20;

/**
 * What an inference template catalog update left out because this server
 * cannot read it yet. The server decides what is skipped; this only lists it.
 */
export default function TemplatesSkippedNotice({ skipped }: { skipped: OfficialInferenceCatalogSkipped[] }) {
  const { t } = useTranslation("settings");
  if (skipped.length === 0) return null;
  return (
    <Notice tone="warning" role="status" title={t("updates.templatesSkipped", { count: skipped.length })}>
      <p>{t("updates.templatesSkippedHelp")}</p>
      <ul aria-label={t("updates.templatesSkippedList")}>
        {skipped.slice(0, shown).map((item) => (
          <li key={`${item.model_id}#${item.spec ?? ""}`} title={item.reason}>
            <code>{item.spec ? `${item.model_id} · ${item.spec}` : item.model_id}</code>
          </li>
        ))}
      </ul>
      {skipped.length > shown && <p>{t("updates.templatesSkippedMore", { count: skipped.length - shown })}</p>}
    </Notice>
  );
}
