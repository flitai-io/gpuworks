import { QuestionCircleOutlined } from "@ant-design/icons";
import { Checkbox, Input, InputNumber, Space, Tooltip, Typography } from "antd";
import type { ExtraParameterPreset, ParameterDefinition, ParameterState, StreamOptionDefinition } from "./types";
import { mergedExtraJSON } from "./extraPreset";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { localizedText } from "../../i18n/localizedText";
import type { PriorityClasses } from "../../api";
import { HelpTip } from "../../ui-kit/HelpTip";
import { OptionSelect } from "../../ui-kit/OptionSelect";
import { classLabel } from "../settings/PrioritySettingsCard";

// The select needs a non-empty value for "ask for no class" (the default).
const DEFAULT_CLASS = "__default__";

/**
 * The request priority class of the model's requests. Shown for a model whose
 * requests take one (the server says which); classes is null while they load.
 */
export interface PriorityChoice {
  classes: PriorityClasses | null;
  /** The engine orders requests by their class; without it the class is only recorded. */
  scheduling: boolean;
}

export function initialParameterState(definitions: ParameterDefinition[], streamOptions: StreamOptionDefinition[] = []): ParameterState {
  return {
    enabled: Object.fromEntries(definitions.map((definition) => [definition.name, false])),
    values: Object.fromEntries(definitions.map((definition) => [definition.name, definition.default])),
    systemPrompt: "",
    extraJSON: "",
    streamOptions: Object.fromEntries(streamOptions.map((option) => [option.name, option.default])),
  };
}

export default function ParametersPanel({
  definitions,
  presets,
  streamOptions = [],
  value,
  onChange,
  priority,
}: {
  definitions: ParameterDefinition[];
  presets: ExtraParameterPreset[];
  streamOptions?: StreamOptionDefinition[];
  value: ParameterState;
  onChange: (value: ParameterState) => void;
  priority?: PriorityChoice;
}) {
  const { t, i18n } = useTranslation("playground");
  const language = i18n.resolvedLanguage || i18n.language;
  const [presetError, setPresetError] = useState("");
  const toggle = (name: string, checked: boolean) => onChange({
    ...value,
    enabled: { ...value.enabled, [name]: checked },
  });
  const setValue = (name: string, next: unknown) => onChange({
    ...value,
    values: { ...value.values, [name]: next },
  });
  const streamOptionOn = (option: StreamOptionDefinition) => value.streamOptions?.[option.name] ?? option.default;
  const setStreamOption = (name: string, checked: boolean) => onChange({
    ...value,
    streamOptions: { ...Object.fromEntries(streamOptions.map((option) => [option.name, streamOptionOn(option)])), [name]: checked },
  });
  const applyPreset = (preset: ExtraParameterPreset) => {
    if (!preset.value) return;
    try {
      const extraJSON = mergedExtraJSON(value.extraJSON, preset.value);
      setPresetError("");
      onChange({ ...value, extraJSON });
    } catch {
      setPresetError(t("parameters.extraPresetRejected"));
    }
  };

  const defaultClass = priority?.classes?.items.find((c) => c.default);
  const priorityOptions = [
    { value: DEFAULT_CLASS, label: defaultClass ? t("parameters.priorityDefault", { name: classLabel(defaultClass) }) : t("parameters.priorityDefaultUnnamed") },
    ...(priority?.classes?.items ?? []).map((c) => ({ value: c.name, label: `${classLabel(c)} (${c.value})` })),
  ];

  return (
    <div className="playground-parameters">
      {priority && (
        <div className="playground-field">
          <span className="flex items-center gap-1">
            <strong id="playground-priority-class-label">{t("parameters.priority")}</strong>
            <HelpTip content={t("parameters.priorityHelp")} />
          </span>
          <OptionSelect
            id="playground-priority-class"
            aria-labelledby="playground-priority-class-label"
            size="sm"
            disabled={!priority.classes}
            options={priorityOptions}
            value={value.priorityClass || DEFAULT_CLASS}
            onChange={(next) => onChange({ ...value, priorityClass: next === DEFAULT_CLASS ? "" : next })}
          />
          {!priority.scheduling && <span className="text-label text-fg-muted">{t("parameters.priorityNotScheduled")}</span>}
        </div>
      )}
      <div className="playground-field">
        <Typography.Text strong>System prompt</Typography.Text>
        <Input.TextArea
          id="playground-system-prompt"
          name="playground-system-prompt"
          rows={3}
          value={value.systemPrompt}
          placeholder={t("parameters.systemPromptPlaceholder")}
          onChange={(event) => onChange({ ...value, systemPrompt: event.target.value })}
        />
      </div>
      <div className="playground-parameter-list">
        {definitions.map((definition) => {
          const enabled = !!value.enabled[definition.name];
          return (
            <div className="playground-parameter" key={definition.name}>
              <Checkbox id={`playground-parameter-enabled-${definition.name}`} name={`playground-parameter-enabled-${definition.name}`} checked={enabled} onChange={(event) => toggle(definition.name, event.target.checked)}>
                {localizedText(definition.label, language)}
              </Checkbox>
              {definition.kind === "string" ? (
                <Input
                  id={`playground-parameter-${definition.name}`}
                  name={`playground-parameter-${definition.name}`}
                  size="small"
                  disabled={!enabled}
                  value={String(value.values[definition.name] ?? "")}
                  onChange={(event) => setValue(definition.name, event.target.value)}
                />
              ) : (
                <InputNumber
                  id={`playground-parameter-${definition.name}`}
                  name={`playground-parameter-${definition.name}`}
                  size="small"
                  disabled={!enabled}
                  min={definition.minimum}
                  max={definition.maximum}
                  step={definition.step}
                  precision={definition.kind === "integer" ? 0 : undefined}
                  value={value.values[definition.name] as number}
                  onChange={(next) => setValue(definition.name, next)}
                />
              )}
            </div>
          );
        })}
      </div>
      {!!streamOptions.length && (
        <div className="playground-field">
          <Typography.Text strong>{t("parameters.streamOptions")}</Typography.Text>
          {streamOptions.map((option) => {
            const required = option.requires ? streamOptions.find((candidate) => candidate.name === option.requires) : undefined;
            // An option whose prerequisite is off has no effect, so it is
            // shown off without losing the user's own choice.
            const blocked = !!required && !streamOptionOn(required);
            return (
              <Checkbox
                key={option.name}
                id={`playground-stream-option-${option.name}`}
                name={`playground-stream-option-${option.name}`}
                checked={streamOptionOn(option) && !blocked}
                disabled={blocked}
                onChange={(event) => setStreamOption(option.name, event.target.checked)}
              >
                <Space size={4}>
                  {localizedText(option.label, language)}
                  <Tooltip title={localizedText(option.description, language)}><QuestionCircleOutlined /></Tooltip>
                </Space>
              </Checkbox>
            );
          })}
        </div>
      )}
      <div className="playground-field">
        <Typography.Text strong>{t("parameters.extra")}</Typography.Text>
        <Space size={[4, 4]} wrap>
          {presets.map((preset) => {
            const label = localizedText(preset.label, language);
            // The server only serves a value when it knows the deployment
            // honors it, so a preset without one cannot be applied: the
            // button stays visible and explains what to add by hand.
            const blocked = !preset.value;
            const reason = blocked ? localizedText(preset.unavailable, language) : "";
            const button = (
              <button
                type="button"
                className="playground-preset"
                disabled={blocked}
                title={reason || undefined}
                aria-label={reason ? `${label}. ${reason}` : label}
                onClick={() => applyPreset(preset)}
              >
                {label}
              </button>
            );
            return (
              <span key={preset.key || label}>
                {blocked ? <Tooltip title={reason}><span>{button}</span></Tooltip> : button}
              </span>
            );
          })}
        </Space>
        {presetError && <p role="alert">{presetError}</p>}
        <Input.TextArea
          id="playground-extra-parameters"
          name="playground-extra-parameters"
          className="playground-json-input"
          rows={5}
          value={value.extraJSON}
          placeholder={'{\n  "reasoning_effort": "low"\n}'}
          onChange={(event) => onChange({ ...value, extraJSON: event.target.value })}
        />
      </div>
    </div>
  );
}
