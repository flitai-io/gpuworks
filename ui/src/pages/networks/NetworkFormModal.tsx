import { useEffect, useState } from "react";
import { Alert, Checkbox, Collapse, Form, Input, Modal, Radio, Tooltip, message } from "antd";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import type { Network, NetworkDetail, NetworkRequest, NetworkRole, NetworkType } from "../../networkTypes";
import { isAcceleratorRDMA, usesRails } from "./networkDisplay";
import { HelpTip } from "../../ui-kit/HelpTip";

type FormValues = Required<Pick<NetworkRequest, "name" | "roles" | "type">> & Omit<NetworkRequest, "name" | "roles" | "type">;

export default function NetworkFormModal({
  open, network, roleOptions, typeOptions, typeRoles, onClose, onSaved,
}: {
  open: boolean;
  network?: Network | null;
  roleOptions: NetworkRole[];
  typeOptions: NetworkType[];
  /** Per type, the roles it may carry; a type missing here may carry any. */
  typeRoles?: Record<string, NetworkRole[]>;
  onClose: () => void;
  onSaved: (network: NetworkDetail) => void;
}) {
  const { t } = useTranslation("networks");
  const [form] = Form.useForm<FormValues>();
  const [saving, setSaving] = useState(false);
  const type = Form.useWatch("type", form);
  const railsType = !!type && usesRails(type);
  const acceleratorType = !!type && isAcceleratorRDMA(type);
  const allowed = (t: NetworkType | undefined, role: NetworkRole) => !t || !typeRoles?.[t] || typeRoles[t].includes(role);

  // Roles the chosen type cannot carry are unticked as well as disabled, so
  // the form never submits a combination the server refuses.
  useEffect(() => {
    if (!open || !type) return;
    const roles: NetworkRole[] = form.getFieldValue("roles") ?? [];
    const kept = roles.filter((role) => allowed(type, role));
    if (kept.length !== roles.length) form.setFieldsValue({ roles: kept });
  }, [open, type, typeRoles, form]);

  useEffect(() => {
    if (!open) return;
    form.resetFields();
    form.setFieldsValue(network
      ? { name: network.name, description: network.description, roles: network.roles, type: network.type, cidr: network.cidr, interface: network.interface }
      : { name: "", description: "", roles: [], type: typeOptions[0] ?? "host_ethernet", cidr: "", interface: "" });
  }, [open, network, form, typeOptions]);

  const submit = async () => {
    let values: FormValues;
    try {
      values = await form.validateFields();
    } catch {
      return; // the form shows the field errors
    }
    // A network that uses rails has no interface glob: its bootstrap is inferred per node.
    const body: NetworkRequest = { ...values, interface: usesRails(values.type) ? "" : values.interface };
    setSaving(true);
    try {
      const saved = network ? await api.updateNetwork(network.id, body) : await api.createNetwork(body);
      message.success(t("message.saved"));
      onSaved(saved);
    } catch (e) {
      message.error(t("message.failed", { detail: (e as Error).message }));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      title={network ? t("form.editTitle") : t("form.createTitle")}
      open={open}
      onCancel={onClose}
      onOk={submit}
      confirmLoading={saving}
      okText={t("actions.save")}
      cancelText={t("actions.cancel")}
      destroyOnHidden
    >
      <Form form={form} layout="vertical">
        <Form.Item label={t("form.name")} name="name" rules={[{ required: true, whitespace: true, message: t("form.nameRequired") }]}>
          <Input maxLength={64} />
        </Form.Item>
        <Form.Item label={t("form.description")} name="description">
          <Input maxLength={256} />
        </Form.Item>
        <Form.Item label={t("form.roles")} name="roles" extra={t("form.rolesHint")} rules={[{ required: true, type: "array", min: 1, message: t("form.rolesRequired") }]}>
          <Checkbox.Group>
            {roleOptions.map((role) => {
              const label = t(`role.${role}`, { defaultValue: role });
              return allowed(type, role)
                ? <Checkbox key={role} value={role}>{label}</Checkbox>
                : (
                  <Tooltip key={role} trigger={["hover", "focus"]} title={t("form.roleNotOnType")}>
                    <span tabIndex={0} aria-label={`${label}: ${t("form.roleNotOnType")}`}>
                      <Checkbox value={role} disabled>{label}</Checkbox>
                    </span>
                  </Tooltip>
                );
            })}
          </Checkbox.Group>
        </Form.Item>
        <Form.Item label={t("form.type")} name="type" extra={<span className="inline-flex items-center gap-1">{t("form.typeHint")}<HelpTip content={t("form.typeHelp")} /></span>}>
          <Radio.Group optionType="button" options={typeOptions.map((value) => ({ value, label: t(`type.${value}`, { defaultValue: value }) }))} />
        </Form.Item>
        {railsType ? (
          <>
            <Alert style={{ marginBottom: 16 }} type="info" showIcon message={t(acceleratorType ? "form.acceleratorInferred" : "form.rdmaInferred")} />
            <Collapse
              ghost
              size="small"
              defaultActiveKey={network?.cidr ? ["advanced"] : []}
              items={[{
                key: "advanced",
                label: t("form.advanced"),
                forceRender: true,
                children: (
                  <Form.Item label={t("form.bootstrapCidr")} name="cidr" extra={t(acceleratorType ? "form.acceleratorBootstrapCidrHint" : "form.bootstrapCidrHint")} style={{ marginBottom: 0 }}>
                    <Input placeholder="10.20.0.0/16" />
                  </Form.Item>
                ),
              }]}
            />
          </>
        ) : (
          <>
            <Form.Item label={t("form.cidr")} name="cidr" extra={t("form.cidrHint")}>
              <Input placeholder="10.20.0.0/16" />
            </Form.Item>
            <Form.Item label={t("form.interface")} name="interface" extra={t("form.interfaceHint")}>
              <Input placeholder={t("form.interfacePlaceholder")} />
            </Form.Item>
            <Alert type="info" showIcon message={t("form.selectorNote")} />
          </>
        )}
      </Form>
    </Modal>
  );
}
