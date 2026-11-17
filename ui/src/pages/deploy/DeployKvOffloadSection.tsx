// KV cache offload tier picker: GPU-resident vs. host-memory vs.
// host-memory+disk, the memory capacity field, the disk tier's companion
// parameters, and one card per KV offload backend the server lists.
import { Form } from "antd";
import { useTranslation } from "react-i18next";
import type { EngineParamSpec, KVBackendProfile } from "../../api";
import DeployParamField from "./DeployParamField";
import { ChoiceCard } from "./DeployChoiceCards";
import { kvBackendKey, memoryTierEnabledField } from "./deployHelpers";

type Props = {
  kvMemorySpec?: EngineParamSpec;
  kvDiskSpec?: EngineParamSpec;
  kvDiskCompanionSpecs: EngineParamSpec[];
  manualCommand: boolean;
  // The engine profile's kv_backends: rendered as they are, never matched
  // against a backend this file knows.
  backends?: KVBackendProfile[];
};

export default function DeployKvOffloadSection({
  kvMemorySpec,
  kvDiskSpec,
  kvDiskCompanionSpecs,
  manualCommand,
  backends = [],
}: Props) {
  const { t } = useTranslation("deploy");
  const form = Form.useFormInstance();
  const kvMemoryValue = Number(
    Form.useWatch(kvMemorySpec?.key || "__missing_kv_memory", {
      form,
      preserve: true,
    }) || 0,
  );
  const kvDiskValue = Boolean(
    Form.useWatch(kvDiskSpec?.key || "__missing_kv_disk", {
      form,
      preserve: true,
    }),
  );
  // The card position is explicit once the deployer clicks; before that the
  // tier follows its value, so a template-provided size lights the card.
  // One watch reads both draft-only positions: the memory card's and the
  // selected KV offload backend, which replaces the engine's own tiers.
  const { kvMemoryIntent, backendValue } =
    Form.useWatch(
      (values: Record<string, unknown>) => ({
        kvMemoryIntent: values[memoryTierEnabledField],
        backendValue: values[kvBackendKey],
      }),
      { form, preserve: true },
    ) ?? {};
  const kvMemoryDesired =
    kvMemoryIntent === undefined ? kvMemoryValue > 0 : Boolean(kvMemoryIntent);
  const kvMemoryEnabled = kvMemoryDesired && kvMemoryValue > 0;
  // The disk tier depends on the memory tier, so its card and companions go
  // dark with it even though their draft values survive.
  const kvDiskEnabled = kvMemoryEnabled && kvDiskValue;
  // A selected backend replaces the engine's own tiers: their cards go dark
  // and picking one of them clears the selection, including one the profile
  // no longer lists (the payload already leaves that one out).
  const backend = backends.find((entry) => entry.id === backendValue);
  const clearBackend = () => {
    if (typeof backendValue === "string" && backendValue !== "") form.setFieldValue(kvBackendKey, "");
  };

  return (
    <div className="deploy-workbench-row">
      <div className="deploy-workbench-label">KV Offload</div>
      <div className="deploy-workbench-body">
        <div className="deploy-choice-strip">
          <ChoiceCard
            active={!backend && !kvMemoryDesired}
            disabled={manualCommand}
            title={t("workbench.kv.disabled")}
            description={t("workbench.kv.gpuResident")}
            onClick={() => {
              clearBackend();
              form.setFieldValue(memoryTierEnabledField, false);
            }}
          />
          <ChoiceCard
            active={!backend && kvMemoryDesired && !kvDiskEnabled}
            disabled={!kvMemorySpec || manualCommand}
            title={t("workbench.kv.memory")}
            description={
              kvMemorySpec
                ? kvMemoryValue > 0
                  ? `${kvMemoryValue} GiB`
                  : t("workbench.kv.enterCapacity")
                : t("workbench.engineUnavailable")
            }
            onClick={() => {
              if (!kvMemorySpec) return;
              clearBackend();
              form.setFieldValue(memoryTierEnabledField, true);
            }}
          />
          <ChoiceCard
            active={!backend && kvDiskEnabled}
            disabled={!kvDiskSpec || !kvMemoryEnabled || manualCommand}
            title={t("workbench.kv.disk")}
            description={
              kvDiskSpec
                ? !kvMemoryEnabled
                  ? t("workbench.kv.memoryRequired")
                  : kvDiskEnabled
                    ? t("workbench.enabled")
                    : t("workbench.clickToEnable")
                : t("workbench.engineUnavailable")
            }
            onClick={() => {
              if (!kvDiskSpec || !kvMemoryEnabled) return;
              clearBackend();
              // The disk tier's switch is the parameter itself: on states it true,
              // off states false, and the value-editable companions ride along.
              form.setFieldValue(kvDiskSpec.key, backend ? true : !kvDiskEnabled);
            }}
          />
          {backends.map((entry) => (
            <ChoiceCard
              key={entry.id}
              active={backend?.id === entry.id}
              disabled={manualCommand}
              title={entry.label}
              description={t("workbench.kv.backendReplaces")}
              onClick={() => form.setFieldValue(kvBackendKey, entry.id)}
            />
          ))}
        </div>
        {!backend && kvMemorySpec && kvMemoryDesired && (
          <div className="deploy-inline-field">
            <DeployParamField
              spec={kvMemorySpec}
              rules={[
                {
                  required: true,
                  message: t("performance.memoryCapacityRequired"),
                },
                {
                  validator: (_, value) =>
                    Number(value) > 0
                      ? Promise.resolve()
                      : Promise.reject(
                          new Error(t("performance.memoryCapacityRequired")),
                        ),
                },
              ]}
            />
          </div>
        )}
        {!backend && kvDiskSpec && kvDiskEnabled && kvDiskCompanionSpecs.length > 0 && (
          <div className="deploy-resource-grid">
            {kvDiskCompanionSpecs.map((spec: EngineParamSpec) => (
              <DeployParamField key={spec.key} spec={spec} />
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
