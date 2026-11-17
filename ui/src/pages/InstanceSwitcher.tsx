import { useTranslation } from "react-i18next";
import { instanceSummaryState, type InstanceChoice } from "./instanceGroups";
import { detailBadgeTone, stateLabel } from "./state";

export function useInstanceChoiceStatus() {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  return function statusOf(inst: InstanceChoice) {
    const state = instanceSummaryState(inst);
    const badge = stateLabel(state, tCommon);
    const label = !inst.state ? t("detail.historicalRecord") : state === "failed" ? t("detail.failedAttempt") : badge.text;
    return { state, label, tone: detailBadgeTone[badge.status] };
  };
}
