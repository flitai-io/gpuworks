import { Form, Select, Tag, Typography } from "antd";
import { useEffect } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { AcquisitionCandidate, AcquisitionResult } from "../../api";
import { storageTypeLabel } from "../../storageDisplay";
import {
  askProvider,
  askSaveTo,
  candidateName,
  lockedProvider,
  resolveProvider,
  resolveSaveTo,
  weightStorageInvalid,
} from "./deployWeightChoices";
import { useControlLocked } from "./DeployEditContext";

const PROVIDER_FIELD = ["model_source", "provider"];
const STORAGE_FIELD = ["model_source", "storage_id"];
// True once the person chose the storage themselves (as opposed to the form
// following the server's recommendation). A chosen storage is never replaced.
const STORAGE_PICKED = ["model_source", "storage_picked"];

function storageName(candidate: AcquisitionCandidate, t: TFunction<"deploy">): string {
  return t("workbench.acquisition.storageTarget", {
    name: candidate.label,
    type: storageTypeLabel(t as unknown as TFunction, candidate.storage_type),
  });
}

/**
 * The model-weights row of the deploy form. It holds the two things a request
 * can say about the files, each shown only when it has a real choice behind it:
 * which of the template's public weight sources to run, and which storage a
 * node saves the model into when it has to fetch it. Where a node gets the
 * files from is not asked; the forecast under the node row says what is
 * expected and the server decides when the replica starts.
 */
export default function DeployWeightSourceFields({
  acquisition,
  modelSource,
  evaluating = false,
}: {
  acquisition?: AcquisitionResult;
  modelSource?: string;
  evaluating?: boolean;
}) {
  const { t } = useTranslation("deploy");
  const form = Form.useFormInstance();
  const sourceLocked = useControlLocked("model_source");
  const provider = Form.useWatch(PROVIDER_FIELD, { form, preserve: true }) as
    | string
    | undefined;
  const storageId = Form.useWatch(STORAGE_FIELD, { form, preserve: true }) as
    | number
    | undefined;
  const storagePicked =
    (Form.useWatch(STORAGE_PICKED, { form, preserve: true }) as
      | boolean
      | undefined) === true;

  // Follow the server's recommendation until the person picks something, and
  // abandon a provider or a recommended storage the latest evaluation no
  // longer offers. A storage the person picked stays, flagged as unavailable,
  // until they choose again, including when fewer than two targets remain.
  useEffect(() => {
    // An edit cannot change where the weights come from.
    if (!acquisition || evaluating || sourceLocked) return;
    // Watchers notify changes, but start undefined when the quick form mounts
    // again. Resolve against the preserved form store before writing defaults.
    const currentProvider = form.getFieldValue(PROVIDER_FIELD);
    const currentStorageId = form.getFieldValue(STORAGE_FIELD);
    const currentStoragePicked = form.getFieldValue(STORAGE_PICKED) === true;
    const wantedProvider = resolveProvider(acquisition, modelSource, currentProvider);
    if (wantedProvider !== currentProvider)
      form.setFieldValue(PROVIDER_FIELD, wantedProvider);
    const wantedStorage = resolveSaveTo(acquisition, currentStorageId, currentStoragePicked);
    if (wantedStorage !== currentStorageId)
      form.setFieldValue(STORAGE_FIELD, wantedStorage);
    if (wantedStorage === undefined && currentStoragePicked)
      form.setFieldValue(STORAGE_PICKED, false);
  }, [acquisition, evaluating, form, modelSource, provider, sourceLocked, storageId, storagePicked]);

  if (!acquisition) return null;
  const providers = acquisition.providers || [];
  const locked = lockedProvider(acquisition, modelSource);
  const showProvider = askProvider(acquisition, modelSource);
  const showSaveTo = askSaveTo(acquisition) || (storagePicked && storageId !== undefined);
  const holders = (acquisition.present || []).filter((c) => !c.unavailable);
  const targets = acquisition.targets || [];
  // A selected destination can become "already there" when its transfer
  // finishes. Keep that selected label without offering new source choices.
  const selectedPresent = storagePicked ? holders.find((c) => c.storage_id === storageId) : undefined;
  const saveOptions = selectedPresent && !targets.some((c) => c.storage_id === storageId) ? [selectedPresent, ...targets] : targets;
  const showLocked = Boolean(locked) && providers.length >= 2;
  if (!showProvider && !showSaveTo && !showLocked && holders.length === 0)
    return null;

  const storageGone = weightStorageInvalid(acquisition, {
    storage_id: storageId,
    storage_picked: storagePicked,
  });

  return (
    <div className="deploy-workbench-row">
      <div className="deploy-workbench-label">
        {t("workbench.acquisition.weightTitle")}
      </div>
      <div className="deploy-workbench-body">
        {holders.length > 0 && (
          <Typography.Text type="secondary" style={{ display: "block" }}>
            {t("workbench.acquisition.storagePresent", {
              list: holders.map((c) => storageName(c, t)).join(", "),
            })}
          </Typography.Text>
        )}
        {showLocked && locked && (
          <Form.Item label={t("workbench.acquisition.modelProvider")}>
            <Typography.Text>{candidateName(locked)}</Typography.Text>
          </Form.Item>
        )}
        {showProvider && (
          <Form.Item
            name={PROVIDER_FIELD}
            label={t("workbench.acquisition.modelProvider")}
          >
            <Select
              options={providers.map((item) => ({
                value: item.label,
                label: item.label,
                title: item.reason,
              }))}
              style={{ minWidth: 220 }}
            />
          </Form.Item>
        )}
        {showSaveTo && (
          <Form.Item
            name={STORAGE_FIELD}
            label={t("workbench.acquisition.saveTo")}
            validateStatus={storageGone ? "error" : undefined}
            help={
              storageGone ? (
                <span role="alert">{t("workbench.acquisition.storageGone")}</span>
              ) : undefined
            }
            getValueProps={(value: number | undefined) => ({
              value: storageGone ? undefined : value,
            })}
          >
            <Select
              onChange={() => form.setFieldValue(STORAGE_PICKED, true)}
              options={saveOptions.map((target) => ({
                value: target.storage_id,
                label: storageName(target, t),
                recommended: Boolean(target.recommended),
              }))}
              optionRender={(option) => (
                <span>
                  {option.label}
                  {(option.data as { recommended?: boolean }).recommended && (
                    <Tag color="green" style={{ marginInlineStart: 8 }}>
                      {t("workbench.acquisition.storageRecommended")}
                    </Tag>
                  )}
                </span>
              )}
              style={{ minWidth: 220 }}
            />
          </Form.Item>
        )}
      </div>
    </div>
  );
}
