import { type ComponentProps, type ReactNode } from "react";
import {
  AutoComplete,
  Form,
  Input,
  InputNumber,
  Segmented,
  Select,
  Slider,
  Space,
  Switch,
  Typography,
} from "antd";
import { Button } from "../../components/ui/button";
import type { EngineParamSpec } from "../../api";
import { localizedText } from "../../i18n/localizedText";
import { useTranslation } from "react-i18next";
import DeployMetricsField from "./DeployMetricsField";
import DeployLogLevelField from "./DeployLogLevelField";
import { useEngineDecides } from "./deployParamApplicability";
import type { Rule } from "antd/es/form";

function NumberControl({
  spec,
  engineDecides,
  ...inputProps
}: ComponentProps<typeof InputNumber<number>> & {
  spec: EngineParamSpec;
  engineDecides: boolean;
}) {
  const { t, i18n } = useTranslation("deploy");
  // A numeric automatic sentinel belongs to the server dictionary. Keep it
  // numeric in the draft and payload, while showing its meaning to people.
  const numericAutomatic = spec.automatic && spec.default === -1;
  const input = (
    <InputNumber
      {...inputProps}
      style={{ width: "100%" }}
      min={spec.min}
      max={spec.max}
      step={spec.step}
      placeholder={spec.automatic
        ? t("shared.automatic")
        : localizedText(spec.placeholder, i18n.resolvedLanguage || i18n.language) || undefined}
      aria-valuetext={numericAutomatic && inputProps.value === -1 ? t("shared.automatic") : undefined}
      formatter={numericAutomatic
        ? (current, info) => info.userTyping
          ? info.input
          : Number(current) === -1 ? t("shared.automatic") : String(current ?? "")
        : undefined}
    />
  );
  if (!numericAutomatic || engineDecides) return input;
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
      {input}
      <Button variant="tertiary" size="md" disabled={inputProps.disabled} onClick={() => inputProps.onChange?.(-1)}>
        {t("shared.automatic")}
      </Button>
    </div>
  );
}

function SliderControl({
  value,
  onChange,
  spec,
  disabled,
  engineDecides,
}: {
  value?: number;
  onChange?: (value: number | undefined) => void;
  spec: EngineParamSpec;
  disabled?: boolean;
  // engineDecides adds the unstated position: no flag is sent and the engine
  // keeps its own value, so the slider shows no number it would not send.
  engineDecides?: boolean;
}) {
  const { t } = useTranslation("deploy");
  const current = Number(value ?? spec.default ?? 0);
  const unstated = engineDecides && value === undefined;
  const position = engineDecides ? (
    <Select
      size="small"
      disabled={disabled}
      aria-label={t("workbench.valueSource")}
      style={{ minWidth: 88 }}
      value={unstated ? "auto" : "set"}
      options={[
        { value: "auto", label: t("workbench.triStateAuto") },
        { value: "set", label: t("workbench.specify") },
      ]}
      onChange={(next) =>
        onChange?.(
          next === "auto" ? undefined : Number(spec.default ?? spec.min ?? 0),
        )
      }
    />
  ) : null;
  if (unstated) {
    return (
      <Space>
        {position}
        <Typography.Text
          type="secondary"
          title={t("workbench.engineDefaultHint")}
        >
          {t("workbench.engineDefault")}
        </Typography.Text>
      </Space>
    );
  }
  return (
    <Space.Compact block>
      {position}
      <Slider
        value={current}
        disabled={disabled}
        onChange={onChange}
        style={{ flex: 1, marginInline: 8 }}
        min={spec.min}
        max={spec.max}
        step={spec.step}
        tooltip={{ formatter: (item) => item?.toFixed(2) }}
      />
      <Typography.Text style={{ width: 44, textAlign: "right" }}>
        {current.toFixed(2)}
      </Typography.Text>
    </Space.Compact>
  );
}

export default function DeployParamField({
  spec,
  rules: additionalRules,
}: {
  spec: EngineParamSpec;
  rules?: Rule[];
}) {
  const { t, i18n } = useTranslation("deploy");
  const form = Form.useFormInstance();
  // A hand-written command is the launch configuration's only source, so the
  // dictionary fields stop editing it and say so by greying out (ADR 0020
  // decision 6). Placement inputs are not dictionary parameters and stay live.
  const manualCommand =
    Form.useWatch("command_mode", { form, preserve: true }) === "manual";
  const isSwitch = spec.type === "boolean" || spec.control === "switch";
  // A key the dictionary gives no default, or whose default this launch
  // leaves to the engine (the entrypoint or release may refuse the flag), is
  // the engine's to decide: the control gets an unstated position it submits,
  // instead of showing a default the command would not carry.
  const engineDecides = useEngineDecides(spec);
  const watched = Form.useWatch(spec.key, { form, preserve: true });
  // A key a deployment left unstated is "cleared" ("") in a form opened from
  // its draft; a switch or slider shows that as its unstated position.
  const value = (isSwitch || spec.control === "slider") && watched === "" ? undefined : watched;
  const language = i18n.resolvedLanguage || i18n.language;
  if (spec.key === "enable_metrics") return <DeployMetricsField spec={spec} />;
  if (spec.key === "log_level")
    return <DeployLogLevelField spec={spec} disabled={manualCommand} />;
  const label = localizedText(spec.label, language);
  const placeholder = localizedText(spec.placeholder, language) || undefined;
  const help = localizedText(spec.help, language) || undefined;
  const options = spec.options?.length
    ? spec.options.map((option) => ({
        value: option.value,
        label: localizedText(option.label, language) || option.value,
      }))
    : (spec.enum ?? []).map((value) => ({ value, label: value }));
  const rules = [...(additionalRules ?? [])];
  let control: ReactNode = (
    <Input disabled={manualCommand} placeholder={placeholder} />
  );
  if (spec.control === "segmented") {
    control = <Segmented block disabled={manualCommand} options={options} />;
  } else if (spec.control === "combobox") {
    control = (
      <AutoComplete
        allowClear
        disabled={manualCommand}
        options={options}
        placeholder={placeholder}
      />
    );
  } else if (spec.control === "slider") {
    control = (
      <SliderControl
        disabled={manualCommand}
        spec={spec}
        value={value}
        engineDecides={engineDecides}
        onChange={(next) => form.setFieldValue(spec.key, next)}
      />
    );
  } else if (isSwitch && !engineDecides) {
    control = (
      <Switch
        aria-label={label}
        disabled={manualCommand}
        checked={value === undefined ? spec.default === true : Boolean(value)}
        onChange={(checked) => form.setFieldValue(spec.key, checked)}
      />
    );
  } else if (isSwitch) {
    // Same control the feature cards use, so an engine-decided boolean reads
    // the same everywhere the form draws it.
    control = (
      <Select
        disabled={manualCommand}
        aria-label={label}
        style={{ minWidth: 104 }}
        value={value === undefined ? "auto" : value ? "on" : "off"}
        options={[
          { value: "auto", label: t("workbench.triStateAuto") },
          { value: "on", label: t("workbench.triStateOn") },
          { value: "off", label: t("workbench.triStateOff") },
        ]}
        onChange={(next) =>
          form.setFieldValue(
            spec.key,
            next === "auto" ? undefined : next === "on",
          )
        }
      />
    );
  } else if (spec.type === "enum" || spec.control === "select") {
    control = (
      <Select
        allowClear={!spec.default || engineDecides}
        disabled={manualCommand}
        options={options}
        placeholder={
          engineDecides && spec.default != null
            ? t("workbench.engineDefault")
            : placeholder
        }
      />
    );
  } else if (
    spec.type === "integer" ||
    spec.type === "number" ||
    spec.control === "number"
  ) {
    control = (
      <NumberControl spec={spec} disabled={manualCommand} engineDecides={engineDecides} />
    );
  } else if (spec.type === "json" || spec.control === "json") {
    control = (
      <Input.TextArea
        autoSize={{ minRows: 2, maxRows: 5 }}
        disabled={manualCommand}
        placeholder={placeholder}
      />
    );
  }
  return (
    <Form.Item
      label={label}
      // An engine-decided boolean registers no field: its Segmented holds the
      // three positions itself and writes the form directly, because "auto" is
      // undefined and a registered field would fall back to a value.
      name={isSwitch || spec.control === "slider" ? undefined : spec.key}
      // The server materializes dictionary defaults. This view displays the
      // effective position without writing it into the draft on mount.
      rules={rules.length ? rules : undefined}
      extra={help}
    >
      {control}
    </Form.Item>
  );
}
