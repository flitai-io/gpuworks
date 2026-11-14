import { PlusOutlined } from "@ant-design/icons";
import {
  Alert,
  Button,
  Col,
  Form,
  Input,
  InputNumber,
  Row,
  Select,
  Space,
  Tag,
  Typography,
} from "antd";
import type { TFunction } from "i18next";
import { useEffect } from "react";
import { useTranslation } from "react-i18next";
import type { EligibleNode, EngineProfile, EvaluatePdUnit, EvaluateResponse, PdRoleName } from "../../api";
import { launchCommand } from "./launchCommand";
import { scheduleText } from "./deploySchedule";
import {
  PD_ROLE_ORDER,
  defaultPdMode,
  pdModeOf,
  pdModesForEngine,
  pdModesForVendor,
  pdPolicyValue,
} from "./deployPd";
import PdUnitRow, { PdAutoUnitRow } from "./PdUnitFields";

function PdRoleSection({
  role,
  eligibleNodes,
  maxUnits,
  auto,
}: {
  role: PdRoleName;
  eligibleNodes?: EligibleNode[];
  maxUnits?: number;
  // The platform places the units: rows count instances, with no node or GPU.
  auto: boolean;
}) {
  const { t } = useTranslation("deploy");
  const roleLabel = t(`pd.role.${role}`);
  const choice = { eligibleNodes };
  return (
    <div className="deploy-node-card">
      <div className="deploy-node-card-head">
        <strong>{roleLabel}</strong>
        <Space size={8}>
          <span>{t("pd.tp")}</span>
          <Form.Item
            name={["pd_roles", role, "tp"]}
            noStyle
            rules={[{ required: true, message: t("pd.tpRequired") }]}
          >
            <InputNumber
              min={1}
              size="small"
              aria-label={t("pd.tpAria", { role: roleLabel })}
            />
          </Form.Item>
          <span>{t("pd.pp")}</span>
          <Form.Item name={["pd_roles", role, "pp"]} noStyle>
            <InputNumber
              min={1}
              size="small"
              aria-label={t("pd.ppAria", { role: roleLabel })}
            />
          </Form.Item>
        </Space>
      </div>
      <Form.List name={["pd_roles", role, "units"]}>
        {(fields, { add, remove }) => (
          <Space direction="vertical" style={{ width: "100%" }} size={6}>
            {fields.map((field, index) =>
              auto ? (
                <PdAutoUnitRow
                  key={field.key}
                  role={role}
                  index={index}
                  removable={fields.length > 1}
                  onRemove={() => remove(field.name)}
                />
              ) : (
                <PdUnitRow
                  key={field.key}
                  role={role}
                  index={index}
                  rootName={["pd_roles", role, "units"]}
                  fieldName={field.name}
                  choice={choice}
                  removable={fields.length > 1}
                  onRemove={() => remove(field.name)}
                />
              ),
            )}
            <Button
              type="dashed"
              size="small"
              icon={<PlusOutlined />}
              disabled={maxUnits != null && fields.length >= maxUnits}
              onClick={() => add({})}
            >
              {t("pd.addUnit", { role: roleLabel })}
            </Button>
          </Space>
        )}
      </Form.List>
    </div>
  );
}

// PdUnitPreview renders one evaluate unit's fit/node/GPUs/address/reason/
// command - shared by a role's own units and, indented, each unit's
// followers (same EvaluatePdUnit shape, ADR 0028's multi-node extension).
function PdUnitPreview({
  t,
  unit,
  indent,
}: {
  t: TFunction<"deploy">;
  unit: EvaluatePdUnit;
  indent?: boolean;
}) {
  const command = launchCommand(unit);
  return (
    <div
      className="deploy-node-item"
      style={{ alignItems: "flex-start", marginLeft: indent ? 20 : 0 }}
    >
      <Tag color={unit.fits ? "green" : "red"}>
        {unit.fits ? t("pd.unitFits") : t("pd.unitDoesNotFit")}
      </Tag>
      <strong>{unit.node_name || unit.node_uuid}</strong>
      <span>
        {(unit.gpus || []).length
          ? unit.gpus!.join(", ")
          : t("pd.gpusAutomatic")}
      </span>
      {unit.address && (
        <Typography.Text type="secondary">{unit.address}</Typography.Text>
      )}
      {!unit.fits && unit.reason && (
        <Typography.Text type="danger">
          {scheduleText(t, unit.reason)}
        </Typography.Text>
      )}
      {command.command && (
        <Typography.Paragraph
          copyable={{ text: command.script }}
          code
          style={{
            fontSize: 12,
            whiteSpace: "pre-wrap",
            wordBreak: "break-all",
            margin: 0,
          }}
        >
          {command.command}
        </Typography.Paragraph>
      )}
    </div>
  );
}

// PdPreview withholds the answer on screen while a re-evaluation runs: that
// answer describes the previous draft (useDeployEvaluation), and its fits and
// commands would contradict the runtime command section, which is already
// waiting for the new one.
function PdPreview({
  result,
  loading,
  auto,
}: {
  result: EvaluateResponse | null;
  loading: boolean;
  auto: boolean;
}) {
  const { t } = useTranslation("deploy");
  const pd = result?.pd;
  if (!pd) return null;
  return (
    <div className="deploy-node-card" data-testid="pd-preview">
      <div className="deploy-node-card-head">
        <strong>{auto ? t("pd.previewAutoTitle") : t("pd.previewTitle")}</strong>
      </div>
      {loading ? (
        <Typography.Text type="secondary">
          {t("catalog.evaluationPending")}
        </Typography.Text>
      ) : pd.roles.map((role) => (
        <div key={role.role} style={{ marginBottom: 12 }}>
          <Typography.Text strong>
            {t(`pd.role.${role.role}`)} · TP {role.tp}
            {role.pp && role.pp > 1 ? ` · PP ${role.pp}` : ""}
          </Typography.Text>
          <Space direction="vertical" style={{ width: "100%" }} size={6}>
            {role.units.map((unit, index) => (
              <div key={`${role.role}-${unit.node_uuid || index}`}>
                <PdUnitPreview t={t} unit={unit} />
                {(unit.followers || []).length > 0 && (
                  <Space direction="vertical" size={4} style={{ width: "100%", marginTop: 4 }}>
                    <Typography.Text type="secondary" style={{ marginLeft: 20, fontSize: 12 }}>
                      {t("pd.followersLabel")}
                    </Typography.Text>
                    {unit.followers!.map((follower, followerIndex) => (
                      <PdUnitPreview
                        key={`${role.role}-${unit.node_uuid || index}-follower-${follower.node_uuid || followerIndex}`}
                        t={t}
                        unit={follower}
                        indent
                      />
                    ))}
                  </Space>
                )}
              </div>
            ))}
          </Space>
        </div>
      ))}
    </div>
  );
}

// DeployPdSection is the whole PD disaggregation editor: the mode pick, the
// decode and prefill role sections (each its own node/GPU/TP picks, or only
// the instance count and TP when the platform places them), the
// gateway policies, and evaluate's per-unit preview. It carries no state of
// its own - every value lives on the surrounding Form under pd_mode,
// pd_roles, pd_fallback and pd_kv_load_failure, projected onto the wire by
// deployPd.ts's buildPdPlacement.
export default function DeployPdSection({
  eligibleNodes,
  result,
  loading,
  profile,
  auto = false,
}: {
  eligibleNodes?: EligibleNode[];
  result: EvaluateResponse | null;
  loading: boolean;
  profile?: EngineProfile;
  // The platform places the units (placement_mode "auto"): the role sections
  // state how many units and their TP, the preview shows where they would go.
  auto?: boolean;
}) {
  const { t } = useTranslation("deploy");
  const form = Form.useFormInstance();
  const vendor = Form.useWatch("target_vendor", { form, preserve: true }) as
    | string
    | undefined;
  const modes = pdModesForVendor(profile, vendor);
  const pdMode = Form.useWatch("pd_mode", form);
  const mode = pdModeOf(profile, pdMode);
  const fallback = Form.useWatch("pd_fallback", form);
  const kvLoadFailure = Form.useWatch("pd_kv_load_failure", form);
  // The policies a mode allows are the mode's own: a pick another mode
  // allowed falls back to this mode's default, and a policy the mode's
  // engine does not have is not sent at all.
  // A mode picked for another vendor gives way to this vendor's default.
  useEffect(() => {
    if (pdMode && modes.length && !modes.some((item) => item.name === pdMode)) {
      form.setFieldValue("pd_mode", defaultPdMode(profile, vendor));
    }
  }, [form, modes, pdMode, profile, vendor]);
  useEffect(() => {
    if (!mode) return;
    const next = {
      pd_fallback: pdPolicyValue(mode.fallbacks, fallback),
      pd_kv_load_failure: pdPolicyValue(mode.kv_load_failures, kvLoadFailure),
    };
    if (next.pd_fallback !== fallback || next.pd_kv_load_failure !== kvLoadFailure) {
      form.setFieldsValue(next);
    }
  }, [form, mode, fallback, kvLoadFailure]);
  return (
    <div className="deploy-node-card" data-testid="pd-section">
      {modes.length === 0 ? (
        <Alert
          type="warning"
          showIcon
          message={
            pdModesForEngine(profile).length
              ? t("workbench.node.pdVendorUnsupported", {
                  vendor: t(`basic.vendor.${vendor}`, {
                    defaultValue: vendor?.toUpperCase(),
                  }),
                })
              : t("pd.engineUnsupported")
          }
        />
      ) : (
        <>
          <Form.Item
            label={t("pd.mode")}
            name="pd_mode"
            rules={[{ required: true, message: t("pd.modeRequired") }]}
          >
            <Select
              options={modes.map((item) => ({ value: item.name, label: item.name }))}
            />
          </Form.Item>
          {mode?.first_request_compiles && (
            <Alert
              type="info"
              showIcon
              message={t("pd.firstRequestCompiles")}
              style={{ marginBottom: 12 }}
            />
          )}
          {/* The field errors the server attributes to "placement.pd" (e.g.
              PD_GPU_OVERLAP) land here: this section is the control they are
              attributed beside, with no single input of its own. */}
          <Form.Item name="pd" hidden>
            <Input />
          </Form.Item>
          <Form.Item noStyle shouldUpdate={() => true}>
            {() => {
              const errors = form.getFieldError("pd");
              return errors.length ? (
                <Alert
                  type="error"
                  showIcon
                  message={errors[0]}
                  style={{ marginBottom: 12 }}
                />
              ) : null;
            }}
          </Form.Item>
          <Row gutter={16}>
            {PD_ROLE_ORDER.map((role) => (
              <Col xs={24} md={12} key={role}>
                <PdRoleSection
                  role={role}
                  eligibleNodes={eligibleNodes}
                  maxUnits={mode?.max_units_per_role}
                  auto={auto}
                />
              </Col>
            ))}
          </Row>
          <Row gutter={16} style={{ marginTop: 12 }}>
            <Col xs={24} md={12}>
              <Form.Item
                label={t("pd.fallback")}
                name="pd_fallback"
                extra={t("pd.fallbackHelp")}
              >
                <Select
                  options={(mode?.fallbacks ?? []).map((value) => ({
                    value,
                    label: t(`pd.fallbackOption.${value}`, { defaultValue: value }),
                  }))}
                />
              </Form.Item>
            </Col>
            {(mode?.kv_load_failures ?? []).length > 0 && (
              <Col xs={24} md={12}>
                <Form.Item
                  label={t("pd.kvLoadFailure")}
                  name="pd_kv_load_failure"
                  extra={t("pd.kvLoadFailureHelp")}
                >
                  <Select
                    options={mode!.kv_load_failures.map((value) => ({
                      value,
                      label: t(`pd.kvLoadFailureOption.${value}`, { defaultValue: value }),
                    }))}
                  />
                </Form.Item>
              </Col>
            )}
          </Row>
          <PdPreview result={result} loading={loading} auto={auto} />
        </>
      )}
    </div>
  );
}
