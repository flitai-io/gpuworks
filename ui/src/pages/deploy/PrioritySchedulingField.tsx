import { useEffect } from "react";
import { useTranslation } from "react-i18next";

import type { PrioritySchedulingEligibility } from "../../api";
import { productMessageText } from "../../apiError";
import { Button } from "../../components/ui/button";
import { Checkbox } from "../../components/ui/checkbox";
import { HelpTip } from "../../ui-kit/HelpTip";

// PrioritySchedulingLabel is the switch's label with its explanation.
export function PrioritySchedulingLabel() {
  const { t } = useTranslation("common");
  return (
    <span className="inline-flex items-center gap-1">
      {t("prioritySchedulingSwitch.title")}
      <HelpTip content={t("prioritySchedulingSwitch.help")} label={t("prioritySchedulingSwitch.helpLabel")} />
    </span>
  );
}

/** withFixArgs is the custom argument text with the server's fix arguments added. */
export function withFixArgs(current: unknown, fixArgs: string[]): string {
  const text = String(current ?? "").trim();
  const addition = fixArgs.join(" ");
  return text ? `${text} ${addition}` : addition;
}

// PrioritySchedulingSwitch is priority_scheduling's control, on one line: the
// box, and when the launch does not allow it the server's reason, with the
// arguments that would allow it offered as one click (onApplyFix adds them to
// the custom arguments, which override the template's flag; the evaluation
// then answers again). Unstated, the server turns it on for a draft whose
// launch allows it, so the box shows exactly that. Whether it does is the
// evaluation's priority_scheduling, which the switch only displays.
export function PrioritySchedulingSwitch({
  value,
  onChange,
  eligibility,
  onApplyFix,
}: {
  value?: boolean;
  onChange?: (value: boolean | undefined) => void;
  // The evaluation's answer; undefined until it has given one, which is not
  // "not allowed": a choice already in the form (a deployment's own) stays.
  eligibility?: PrioritySchedulingEligibility;
  onApplyFix: (fixArgs: string[]) => void;
}) {
  const { t } = useTranslation("common");
  const allowed = eligibility?.ok;
  useEffect(() => {
    // A choice made for a launch that allowed it does not carry over to one
    // that does not; the server's default takes over again.
    if (allowed === false && value === true) onChange?.(undefined);
  }, [allowed, value, onChange]);
  const refused = eligibility && !eligibility.ok ? eligibility : undefined;
  const fixArgs = refused?.fix_args ?? [];
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
      <label className="inline-flex items-center gap-2">
        <Checkbox
          checked={allowed === true && (value ?? true)}
          disabled={allowed !== true}
          onCheckedChange={(checked) => onChange?.(checked === true)}
        />
        {t("prioritySchedulingSwitch.enable")}
      </label>
      {refused?.refusal && (
        <span className="text-label text-fg-muted" data-testid="priority-scheduling-refusal">
          {productMessageText(t, { code: refused.refusal.code, params: refused.refusal.params, message: refused.refusal.code })}
        </span>
      )}
      {fixArgs.length > 0 && (
        <span className="inline-flex items-center gap-1">
          <Button type="button" size="sm" variant="link" onClick={() => onApplyFix(fixArgs)}>
            {t("prioritySchedulingSwitch.fix", { args: fixArgs.join(" ") })}
          </Button>
          <HelpTip content={t("prioritySchedulingSwitch.fixTradeoff")} />
        </span>
      )}
    </div>
  );
}
