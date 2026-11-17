import type { ReactElement } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "../../components/ui/button";
import { DropdownContent, DropdownMenu, DropdownSeparator, DropdownTrigger } from "../../components/ui/dropdown";
import { MenuItem } from "../../components/ui/menu-item";
import { Tooltip } from "../../components/ui/tooltip";
import type { ConsoleIcon } from "../../ui-kit/consoleIcons";
import { ConsoleIcon as Icons } from "../../ui-kit/consoleIcons";

/** One action of a row. */
export interface RowAction {
  key: string;
  label: string;
  icon: (typeof ConsoleIcon)[keyof typeof ConsoleIcon];
  onSelect: () => void;
  disabled?: boolean;
  /** Why it is disabled, said on hover. */
  hint?: string;
  /** Destructive: drawn red, and listed last under a rule when folded. */
  danger?: boolean;
  /** Kept beside the menu when the row's actions are folded. */
  common?: boolean;
  loading?: boolean;
}

// A row shows this many icon buttons before the rest fold into a menu.
const SHOWN = 3;

/**
 * A row's actions as icon buttons, each with its name on hover. A row with
 * more than three keeps its common ones out and folds the rest into a "More"
 * menu, the destructive ones last. Confirming an action is the action's own
 * concern: from a menu it has to be a dialog, since the menu closes.
 */
export default function RowActions({ actions, label }: { actions: RowAction[]; label: string }) {
  const { t } = useTranslation("common");
  const fold = actions.length > SHOWN;
  const shown = fold ? actions.filter((a) => a.common) : actions;
  const folded = fold ? actions.filter((a) => !a.common) : [];
  const plain = folded.filter((a) => !a.danger);
  const danger = folded.filter((a) => a.danger);
  const button = (a: RowAction): ReactElement => (
    <Tooltip key={a.key} content={a.disabled && a.hint ? a.hint : a.label}>
      <span className="inline-flex">
        <Button variant="ghost" size="sm" iconOnly aria-label={a.label} disabled={a.disabled} loading={a.loading}
          className={a.danger ? "text-fg-danger hover:text-fg-danger" : undefined} onClick={a.onSelect}>
          <a.icon size={16} />
        </Button>
      </span>
    </Tooltip>
  );
  const item = (a: RowAction, index: number) => (
    <MenuItem key={a.key} index={index} icon={a.icon} label={a.label} disabled={a.disabled} title={a.disabled ? a.hint : undefined}
      className={a.danger ? "text-fg-danger [&_*]:text-fg-danger" : undefined} onSelect={a.onSelect} />
  );
  return (
    <div className="flex items-center gap-0.5" role="group" aria-label={label}>
      {shown.map(button)}
      {folded.length > 0 && (
        <DropdownMenu>
          <Tooltip content={t("actions.more")}>
            <span className="inline-flex">
              <DropdownTrigger render={<Button variant="ghost" size="sm" iconOnly aria-label={t("actions.more")}><Icons.more size={16} /></Button>} />
            </span>
          </Tooltip>
          <DropdownContent align="end">
            {plain.map((a, i) => item(a, i))}
            {plain.length > 0 && danger.length > 0 && <DropdownSeparator />}
            {danger.map((a, i) => item(a, plain.length + i))}
          </DropdownContent>
        </DropdownMenu>
      )}
    </div>
  );
}
