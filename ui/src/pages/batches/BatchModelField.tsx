import { Notice } from "../../ui-kit/Notice";
import { Checkbox } from "../../components/ui/checkbox";
import { OptionSelect } from "../../ui-kit/OptionSelect";
import { useTranslation } from "react-i18next";
import type { BatchModel } from "../../batchApi";

// modelStateKey is the locale key of each state the server sends.
export const modelStateKey: Record<string, string> = {
  running: "running", asleep: "asleep", starting: "starting", asleep_held: "asleepHeld", offline: "offline", failed: "failed", stopped: "stopped",
};

/** The words of a model's state; the state is the server's (the batch dictionary). */
export function useModelStateText() {
  const { t } = useTranslation("batches");
  return (state: string): string => (modelStateKey[state] ? t(`create.model.state.${modelStateKey[state]}`) : state);
}

/** The words of what becomes of a batch sent to a model in this state; none for a state whose requests are served. */
export function useModelStateWarning() {
  const { t } = useTranslation("batches");
  return (model: string, state: string): string | null => {
    const key = modelStateKey[state];
    return key && t(`create.model.warning.${key}`, { model, defaultValue: "" }) || null;
  };
}

/**
 * BatchModelField picks the model every request of the batch is sent to, or
 * none to use each line's own. The server's list is offered with each model's
 * state; one whose requests would wait for someone to act is warned about.
 */
export default function BatchModelField({ options, value, onChange, disabled, showWaiting, onShowWaitingChange }: {
  options: BatchModel[];
  value: string | undefined;
  onChange: (model: string | undefined) => void;
  disabled: boolean;
  showWaiting: boolean;
  onShowWaitingChange: (show: boolean) => void;
}) {
  const { t } = useTranslation("batches");
  const stateText = useModelStateText();
  const stateWarning = useModelStateWarning();
  const chosen = options.find((option) => option.name === value);
  const warning = chosen ? stateWarning(chosen.name, chosen.state) : null;
  return (
    <div className="batch-create-field">
      <label htmlFor="batch-create-model">{t("create.model.label")}</label>
      <OptionSelect
        id="batch-create-model"
        clearable
        value={value}
        onChange={(next) => onChange(next || undefined)}
        placeholder={t("create.model.placeholder")}
        disabled={disabled}
        options={options.map((option) => ({ value: option.name, label: `${option.name} · ${stateText(option.state)}` }))}
        style={{ width: "100%" }}
      />
      <label className="flex items-center gap-2 px-1.5"><Checkbox checked={showWaiting} onCheckedChange={(checked) => onShowWaitingChange(checked === true)} disabled={disabled} />
        {t("create.model.showWaiting")}
      </label>
      <span className="batch-create-help text-label text-fg-muted">{t("create.model.help")}</span>
      {warning && <Notice tone="warning" title={warning} />}
    </div>
  );
}
