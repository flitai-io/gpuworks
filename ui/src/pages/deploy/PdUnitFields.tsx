import { DeleteOutlined, PlusOutlined } from "@ant-design/icons";
import { Button, Col, Form, Row, Select, Space, Tag, Typography } from "antd";
import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";
import type { EligibleNode, Node, PdRoleName } from "../../api";
import { useNodeLookup } from "../../nodes/useNodeLookup";
import EligibleNodePicker from "./EligibleNodePicker";
import { gpuSelectOptions } from "./gpuOptions";

// PdNodeChoice is evaluate's per-node eligibility for the draft a PD row
// belongs to. A caller with no evaluate result at all (the deployment
// detail's PD scale modal, which never evaluates) leaves it undefined and
// every online node is offered enabled.
export type PdNodeChoice = { eligibleNodes?: EligibleNode[] };

// PdNodePicker is a row's node control: online nodes searched on the server.
function PdNodePicker({ choice, ariaLabel, value, onChange, placeholder, allowClear }: { choice: PdNodeChoice; ariaLabel: string; value?: string; onChange?: (value: unknown) => void; placeholder?: string; allowClear?: boolean }) {
  const { t } = useTranslation("deploy");
  return (
    <EligibleNodePicker
      value={value}
      onChange={onChange}
      eligibleNodes={choice.eligibleNodes}
      placeholder={placeholder ?? t("pd.selectNode")}
      allowClear={allowClear}
      aria-label={ariaLabel}
      style={{ width: "100%" }}
    />
  );
}

function gpuOptions(node: Node | undefined, tc: TFunction) {
  return gpuSelectOptions(node?.gpus, tc);
}

// PdFollowerRow is one headless rank of a multi-node unit: another node
// (plus optional GPUs) alongside the unit's own node, in rank order. It has
// no TP/PP of its own - the unit's role states those. followerRootName is
// the follower Form.List's own absolute path ([...unitRootName, fieldName,
// "followers"]), the same "absolute path for Form.useWatch, relative name
// for Form.Item" split PdUnitRow itself uses.
function PdFollowerRow({
  role,
  unitIndex,
  followerIndex,
  followerRootName,
  followerFieldName,
  choice,
  onRemove,
}: {
  role: PdRoleName;
  unitIndex: number;
  followerIndex: number;
  followerRootName: (string | number)[];
  followerFieldName: number;
  choice: PdNodeChoice;
  onRemove: () => void;
}) {
  const { t } = useTranslation("deploy");
  const { t: tc } = useTranslation("common");
  const form = Form.useFormInstance();
  const roleLabel = t(`pd.role.${role}`);
  const nodeUUID = Form.useWatch(
    [...followerRootName, followerFieldName, "node_uuid"],
    { form, preserve: true },
  ) as string | undefined;
  const node = useNodeLookup([nodeUUID]).get(nodeUUID ?? "");
  const ariaArgs = { role: roleLabel, index: unitIndex + 1, follower: followerIndex + 1 };
  return (
    <Row gutter={8} align="middle" className="deploy-pd-follower-row" wrap={false}>
      <Col flex="0 0 auto" style={{ paddingLeft: 24 }}>
        <Typography.Text type="secondary">+</Typography.Text>
      </Col>
      <Col flex="1 1 200px">
        <Form.Item
          name={[followerFieldName, "node_uuid"]}
          noStyle
          rules={[{ required: true, whitespace: true, message: t("pd.unitNodeRequired") }]}
        >
          <PdNodePicker choice={choice} ariaLabel={t("pd.followerNodeAria", ariaArgs)} />
        </Form.Item>
      </Col>
      <Col flex="1 1 200px">
        <Form.Item name={[followerFieldName, "gpus"]} noStyle>
          <Select
            mode="multiple"
            allowClear
            placeholder={t("pd.autoGpus")}
            aria-label={t("pd.followerGpuAria", ariaArgs)}
            style={{ width: "100%" }}
            options={gpuOptions(node, tc)}
          />
        </Form.Item>
      </Col>
      <Col flex="0 0 auto">
        <Button
          aria-label={t("pd.removeFollower", ariaArgs)}
          icon={<DeleteOutlined />}
          onClick={onRemove}
        />
      </Col>
    </Row>
  );
}

// PdAutoUnitRow is one role's instance row in a deployment the platform
// places: no node and no GPU pickers - the platform picks both when the unit
// starts, and the preview beside the roles shows where it would go now. The
// row only counts the instance, so it can be removed.
export function PdAutoUnitRow({
  role,
  index,
  onRemove,
  removable,
}: {
  role: PdRoleName;
  index: number;
  onRemove: () => void;
  removable: boolean;
}) {
  const { t } = useTranslation("deploy");
  const roleLabel = t(`pd.role.${role}`);
  return (
    <Row gutter={8} align="middle" className="deploy-pd-unit-row" wrap={false}>
      <Col flex="1 1 auto">
        <Typography.Text>{t("pd.autoUnit", { role: roleLabel, index: index + 1 })}</Typography.Text>{" "}
        <Tag>{t("pd.autoScheduled")}</Tag>
      </Col>
      <Col flex="0 0 auto">
        <Button
          aria-label={t("pd.removeUnit", { role: roleLabel, index: index + 1 })}
          icon={<DeleteOutlined />}
          onClick={onRemove}
          disabled={!removable}
        />
      </Col>
    </Row>
  );
}

// PdUnitRow is one role's instance row: node picker, GPU picker, remove
// button, and - once it has at least one follower - the headless ranks
// alongside it (a multi-node unit) with an "add node" action and a hint that
// the role's TP must be set. Shared by the deploy form's PD section
// (DeployPdSection.tsx, rootName ["pd_roles", role, "units"]) and the
// deployment detail's PD scale modal (DeploymentPdScaleModal.tsx, rootName
// ["roles", role, "units"]) - rootName is the enclosing Form.List's own
// absolute field path, needed here only to watch the row's picked node (to
// look up its GPUs), since Form.Item names below are already scoped
// relative to that Form.List.
export default function PdUnitRow({
  role,
  index,
  rootName,
  fieldName,
  choice,
  onRemove,
  removable,
  placed,
  isNew = false,
}: {
  role: PdRoleName;
  index: number;
  rootName: (string | number)[];
  fieldName: number;
  choice: PdNodeChoice;
  onRemove: () => void;
  removable: boolean;
  // The platform places the deployment ("auto"): a unit's node is optional
  // (a stated one is the unit's first placement, an unstated one is picked
  // for it), and no card is pinned. A manual deployment names a node per unit.
  placed?: "auto" | "manual";
  // A unit added in this dialog; only these say whether the node is named.
  isNew?: boolean;
}) {
  const { t } = useTranslation("deploy");
  const { t: tc } = useTranslation("common");
  const form = Form.useFormInstance();
  const roleLabel = t(`pd.role.${role}`);
  const nodeUUID = Form.useWatch(
    [...rootName, fieldName, "node_uuid"],
    { form, preserve: true },
  ) as string | undefined;
  const node = useNodeLookup([nodeUUID]).get(nodeUUID ?? "");
  const followersRootName = [...rootName, fieldName, "followers"];
  const auto = placed === "auto";
  // A new unit of an automatic deployment: named or left to the platform.
  const autoNew = auto && isNew;
  return (
    <Space direction="vertical" style={{ width: "100%" }} size={4}>
      <Row gutter={8} align="middle" className="deploy-pd-unit-row" wrap={false}>
        <Col flex="1 1 200px">
          <Form.Item
            name={[fieldName, "node_uuid"]}
            noStyle
            rules={auto ? undefined : [{ required: true, message: t("pd.unitNodeRequired") }]}
          >
            <PdNodePicker
              choice={choice}
              ariaLabel={t("pd.unitNodeAria", { role: roleLabel, index: index + 1 })}
              placeholder={autoNew ? t("pd.preferNodePlaceholder") : undefined}
              allowClear={autoNew}
            />
          </Form.Item>
        </Col>
        {autoNew && (
          <Col flex="0 0 auto">
            <Tag color={nodeUUID ? "blue" : "default"}>
              {nodeUUID ? t("pd.nodeNamed") : t("pd.autoScheduled")}
            </Tag>
          </Col>
        )}
        {!auto && (
          <Col flex="1 1 200px">
            <Form.Item name={[fieldName, "gpus"]} noStyle>
              <Select
                mode="multiple"
                allowClear
                placeholder={t("pd.autoGpus")}
                aria-label={t("pd.unitGpuAria", { role: roleLabel, index: index + 1 })}
                style={{ width: "100%" }}
                options={gpuOptions(node, tc)}
              />
            </Form.Item>
          </Col>
        )}
        <Col flex="0 0 auto">
          <Button
            aria-label={t("pd.removeUnit", { role: roleLabel, index: index + 1 })}
            icon={<DeleteOutlined />}
            onClick={onRemove}
            disabled={!removable}
          />
        </Col>
      </Row>
      {!autoNew && (
      <Form.List name={[fieldName, "followers"]}>
        {(followerFields, { add: addFollower, remove: removeFollower }) => (
          <Space direction="vertical" style={{ width: "100%" }} size={4}>
            {followerFields.length > 0 && (
              <Typography.Text type="warning" style={{ paddingLeft: 24, fontSize: 12 }}>
                {t("pd.multiNodeTpHint", { role: roleLabel })}
              </Typography.Text>
            )}
            {followerFields.map((followerField, followerIndex) => (
              <PdFollowerRow
                key={followerField.key}
                role={role}
                unitIndex={index}
                followerIndex={followerIndex}
                followerRootName={followersRootName}
                followerFieldName={followerField.name}
                choice={choice}
                onRemove={() => removeFollower(followerField.name)}
              />
            ))}
            <Button
              type="dashed"
              size="small"
              icon={<PlusOutlined />}
              style={{ marginLeft: 24 }}
              onClick={() => addFollower({})}
            >
              {t("pd.addNode", { role: roleLabel, index: index + 1 })}
            </Button>
          </Space>
        )}
      </Form.List>
      )}
    </Space>
  );
}
