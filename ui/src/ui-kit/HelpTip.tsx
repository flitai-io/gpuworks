import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import InformationCircle from "@hugeicons/core-free-icons/InformationCircleIcon";
import { createHugeIcon } from "@/lib/huge-icon";
import { Tooltip } from "../components/ui/tooltip";

const InfoIcon = createHugeIcon(InformationCircle);

/**
 * The ⓘ beside a label or value that holds its explanation: formulas,
 * caveats, data sources. docs/dev/ui.md "Copy on the page" keeps such text
 * off the page; this is where it goes. The trigger is a focusable button so
 * keyboard users reach the text too.
 */
export function HelpTip({ content, label, className }: { content: ReactNode; label?: string; className?: string }) {
  const { t } = useTranslation("common");
  return <Tooltip content={<div className="max-w-80 whitespace-pre-line font-normal">{content}</div>}>
    <button
      type="button"
      aria-label={label ?? t("helpTip.label")}
      className={["inline-flex size-4 shrink-0 cursor-help items-center justify-center align-middle text-fg-subtle hover:text-fg-default rounded-full focus-visible:ring-1 focus-visible:ring-focus-ring", className].filter(Boolean).join(" ")}
    >
      <InfoIcon size={14} aria-hidden="true" />
    </button>
  </Tooltip>;
}
