import {
  Alert,
  Button,
  Form,
  Input,
  Popconfirm,
  Space,
  Typography,
} from "antd";
import { EditOutlined, UndoOutlined } from "@ant-design/icons";
import type {
  EvaluateResponse,
  EvaluateRankCommand,
  EvaluatePdUnit,
} from "../../api";
import { useTranslation } from "react-i18next";
import { launchCommand } from "./launchCommand";
import CopyButton from "../../components/CopyButton";
import DeployMountPreview, { mountClipboardText } from "./DeployMountPreview";
import { warningText } from "./deployWarnings";

// PdLaunch is one rank of a PD role unit, labelled by role, instance and
// node: a unit's ranks each run on their own node, so the node tells a
// multi-node unit's leader and followers apart.
type PdLaunch = EvaluatePdUnit & { pd_label: string };

export default function DeployRuntimeCommand({
  result,
  loading,
  error,
}: {
  result: EvaluateResponse | null;
  loading: boolean;
  error: string | null;
}) {
  const { t } = useTranslation("deploy");
  const form = Form.useFormInstance();
  const commandMode = Form.useWatch("command_mode", { form, preserve: true });
  const manual = commandMode === "manual";
  // A PD draft previews one launch per rank of every role unit (decode
  // first, then prefill; a multi-node unit's follower ranks after its own);
  // it replaces the ordinary single/rank preview rather than adding to it -
  // the top-level command fields already describe only the first decode
  // instance.
  const pdLaunches: PdLaunch[] =
    result?.pd?.roles.flatMap((role) =>
      role.units.flatMap((unit, index) =>
        [unit, ...(unit.followers || [])].map((rank) => ({
          ...rank,
          pd_label: `${t(`pd.role.${role.role}`)} #${index + 1} · ${rank.node_name || rank.node_uuid}`,
        })),
      ),
    ) ?? [];
  const launches: Array<EvaluateResponse | EvaluateRankCommand | PdLaunch> =
    pdLaunches.length
      ? pdLaunches
      : result?.rank_commands?.length
        ? result.rank_commands
        : result
          ? [result]
          : [];
  const ready = !loading && !error;
  const multiRank = launches.length > 1;
  const singleLaunch = launches.length === 1 ? launches[0] : undefined;
  // Several launches share one manual command: the server writes the script
  // they start from, with the platform variables it fills in per rank, and
  // says why when it cannot (MANUAL_DRAFT_UNAVAILABLE).
  const draft = multiRank ? result?.manual_command_draft || "" : "";
  const generated = singleLaunch ? launchCommand(singleLaunch).script : draft;
  const generatedPreview = singleLaunch
    ? [generated, mountClipboardText(singleLaunch.mounts || [])]
        .filter(Boolean)
        .join("\n")
    : draft;
  const canEdit = ready && Boolean(generated);
  const draftUnavailable = result?.warnings?.find(
    (warning) => warning.code === "MANUAL_DRAFT_UNAVAILABLE",
  );
  const variables = result?.manual_command_variables ?? [];

  const enterManualMode = () =>
    form.setFieldsValue({
      command_edit_previous_mode: commandMode || "engine",
      command_mode: "manual",
      // A single launch's script carries its own preparation; the shared
      // draft does not, so the platform keeps preparing every rank.
      command_includes_preparation: !multiRank,
      container_cmd: generated,
    });
  const returnToForm = () =>
    form.setFieldsValue({
      command_mode:
        form.getFieldValue("command_edit_previous_mode") || "engine",
      command_edit_previous_mode: undefined,
      command_includes_preparation: undefined,
      container_cmd: undefined,
    });
  return (
    <section className="deploy-runtime-command">
      <div className="deploy-runtime-command-heading">
        <div>
          <Typography.Text strong>{t("runtimeCommand.title")}</Typography.Text>
          <Typography.Text type="secondary">
            {t("runtimeCommand.description")}
          </Typography.Text>
          {result && (
            <Typography.Text type="warning">
              {t("runtimeCommand.dynamicBindings")}
            </Typography.Text>
          )}
        </div>
        <Space>
          {!manual ? (
            <Popconfirm
              title={t("runtimeCommand.enterTitle")}
              description={t("runtimeCommand.enterDescription")}
              okText={t("runtimeCommand.enter")}
              cancelText={t("shared.cancel")}
              onConfirm={enterManualMode}
              disabled={!canEdit}
            >
              <Button icon={<EditOutlined />} disabled={!canEdit}>
                {t("runtimeCommand.edit")}
              </Button>
            </Popconfirm>
          ) : (
            <Popconfirm
              title={t("runtimeCommand.returnTitle")}
              description={t("runtimeCommand.returnDescription")}
              okText={t("runtimeCommand.discardAndReturn")}
              cancelText={t("runtimeCommand.continueEditing")}
              onConfirm={returnToForm}
            >
              <Button icon={<UndoOutlined />}>
                {t("runtimeCommand.return")}
              </Button>
            </Popconfirm>
          )}
          <CopyButton
            type="default"
            label={t("runtimeCommand.copyAria")}
            disabled={!manual && (!ready || !generated)}
            text={() =>
              manual
                ? String(form.getFieldValue("container_cmd") || "")
                : generatedPreview
            }
          />
        </Space>
      </div>
      {multiRank && (
        <Alert
          type="info"
          showIcon
          message={
            manual
              ? t("catalog.sharedManualCommand")
              : draft
                ? t("catalog.multiRankManualDraft")
                : draftUnavailable
                  ? warningText(t, draftUnavailable)
                  : t("catalog.multiRankManualUnavailable")
          }
          description={
            manual &&
            variables.length > 0 && (
              <p className="deploy-manual-variables">
                {t("runtimeCommand.variablesHint")}{" "}
                {variables.map((name) => (
                  <code key={name}>{`\${${name}}`}</code>
                ))}
              </p>
            )
          }
        />
      )}
      {manual && (
        <>
          <Alert
            type="warning"
            showIcon
            message={t("runtimeCommand.manualWarning")}
          />
          <Form.Item
            name="container_cmd"
            rules={[{ required: true, whitespace: true, message: t("runtimeCommand.required") }]}
          >
            <Input.TextArea
              autoSize={{ minRows: 3, maxRows: 10 }}
              className="deploy-command-editor"
            />
          </Form.Item>
        </>
      )}
      {manual && !multiRank ? null : loading || error || !result ? (
        <pre>
          {loading
            ? t("runtimeCommand.generating")
            : error
              ? t("runtimeCommand.afterChecks")
              : t("runtimeCommand.afterSelection")}
        </pre>
      ) : (
        launches.map((launch, index) => {
          const text = launchCommand(launch);
          const copied = [text.script, mountClipboardText(launch.mounts || [])]
            .filter(Boolean)
            .join("\n");
          const rank =
            "pd_label" in launch
              ? launch.pd_label
              : "node_rank" in launch
                ? launch.node_rank
                : index;
          return (
            <div className="deploy-rank-launch" key={index}>
              {launches.length > 1 && (
                <Space>
                  <Typography.Text>rank {rank}</Typography.Text>
                  <CopyButton
                    size="small"
                    type="default"
                    label={t("catalog.copyRank", { rank })}
                    text={copied}
                  />
                </Space>
              )}
              <pre>{text.command}</pre>
              {text.environment && (
                <details>
                  <summary>{t("runtimeCommand.environment")}</summary>
                  <pre>{text.environment}</pre>
                </details>
              )}
              {text.preparation && (
                <details>
                  <summary>{t("runtimeCommand.preparation")}</summary>
                  <pre>{text.preparation}</pre>
                </details>
              )}
              <DeployMountPreview
                mounts={launch.mounts || []}
                copyLabel={
                  launches.length > 1
                    ? t("runtimeCommand.copyMountsAria", { rank })
                    : undefined
                }
              />
              <details>
                <summary>{t("catalog.resolvedRuntime")}</summary>
                <pre>
                  {JSON.stringify(
                    {
                      // A PD unit's preview (EvaluatePdUnit) carries no
                      // image/command_mode/model_mount_path of its own - the
                      // top-level evaluate answer already states those for
                      // the whole draft, once, since every PD instance
                      // shares them.
                      image: "image" in launch ? launch.image : undefined,
                      command_mode:
                        "command_mode" in launch
                          ? launch.command_mode
                          : undefined,
                      model_mount_path:
                        "model_mount_path" in launch
                          ? launch.model_mount_path
                          : undefined,
                      mounts: launch.mounts || [],
                      docker_args: launch.docker_args || [],
                      shm_bytes: launch.shm_bytes,
                    },
                    null,
                    2,
                  )}
                </pre>
              </details>
            </div>
          );
        })
      )}
    </section>
  );
}
