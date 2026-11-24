import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Notice } from "../../ui-kit/Notice";

export default function ImageCatalogConflicts({ skipped, children, action }: { skipped: string[]; children?: ReactNode; action?: ReactNode }) {
  const { t } = useTranslation("images");
  if (!skipped.length) return null;
  return (
    <Notice tone="warning" role="status" title={t("page.catalog.conflicts", { n: skipped.length })} action={action}>
      <details>
        <summary>{t("page.catalog.conflictNames")}</summary>
        <ul>{skipped.map((name, index) => <li key={index}><code>{name}</code></li>)}</ul>
      </details>
      {children}
    </Notice>
  );
}
