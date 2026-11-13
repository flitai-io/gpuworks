import { useEffect, useRef, useState } from "react";
import { Alert, Form, Input, Modal, Select, message } from "antd";
import {
  api,
  type CloudProvider,
  type ProviderCredentialField,
  type ProviderKindInfo,
} from "../api";
import { useTranslation } from "react-i18next";
import { localizedText } from "../i18n/localizedText";

export default function ProviderConfigModal({
  open,
  provider,
  defaultKind,
  kindInfos,
  providerLabel,
  onClose,
  onSaved,
}: {
  open: boolean;
  provider?: CloudProvider | null;
  defaultKind?: string | null;
  kindInfos: ProviderKindInfo[];
  providerLabel: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t, i18n } = useTranslation("providers");
  const language = i18n.resolvedLanguage || i18n.language;
  const [form] = Form.useForm();
  const [kind, setKind] = useState<string>("");
  const [submitting, setSubmitting] = useState(false);
  const [loadedProviderID, setLoadedProviderID] = useState<number | null>(null);
  const [loadError, setLoadError] = useState(false);
  const editing = !!provider;
  const editReady = !provider || (loadedProviderID === provider.id && kindInfos.some((info) => info.kind === provider.kind));
  const selectedKind = kindInfos.find((k) => k.kind === kind);
  const selectedKindLabel = selectedKind ? t(`kind.${selectedKind.kind}`, { defaultValue: localizedText(selectedKind.label, language) || selectedKind.kind }) : "provider";
  const requirement = t(`config.requirements.${provider?.kind ?? kind}`, { defaultValue: "" });

  // The initial kind is applied once per open (and again when the provider or
  // the preselected kind changes), never in reaction to the user's own pick:
  // the seed key remembers what was applied, so a choice in the Type select
  // is not snapped back to the first kind.
  const seededRef = useRef("");
  useEffect(() => {
    if (!open) {
      seededRef.current = "";
      return;
    }
    if (kindInfos.length === 0) return;
    const seed = `${provider?.id ?? ""}|${provider?.kind ?? ""}|${defaultKind ?? ""}`;
    if (seededRef.current === seed) return;
    seededRef.current = seed;
    const next = provider?.kind
      ? provider.kind
      : defaultKind && kindInfos.some((k) => k.kind === defaultKind)
        ? defaultKind
        : kindInfos[0].kind;
    setKind(next);
    if (provider) {
      if (loadedProviderID === provider.id) return;
      form.setFieldsValue({
        name: provider.name,
        endpoint: undefined,
        public_key: undefined,
        api_key: undefined,
        private_key: undefined,
        extra: {},
      });
    } else {
      form.setFieldsValue({
        kind: next,
        name: undefined,
        endpoint: undefined,
        api_key: undefined,
        public_key: undefined,
        private_key: undefined,
        extra: {},
        ...credentialDefaults(kindInfos.find((info) => info.kind === next)),
      });
    }
  }, [open, provider, defaultKind, kindInfos, form, loadedProviderID]);

  // Fetch on every edit opening so refreshes and other sessions cannot leave
  // stale overrides in the form. A closed/replaced dialog ignores late replies.
  const providerID = provider?.id;
  useEffect(() => {
    setLoadedProviderID(null);
    setLoadError(false);
    if (!open || providerID === undefined) return;
    let cancelled = false;
    api.provider(providerID).then((config) => {
      if (cancelled) return;
      form.setFieldsValue({
        name: config.name,
        endpoint: config.endpoint,
        public_key: config.public_key,
        extra: config.extra,
      });
      setLoadedProviderID(providerID);
    }).catch(() => {
      if (!cancelled) setLoadError(true);
    });
    return () => { cancelled = true; };
  }, [open, providerID, form]);

  const submit = async () => {
    if (!editReady || loadError || submitting) return;
    let v;
    try {
      v = await form.validateFields();
    } catch {
      return;
    }
    setSubmitting(true);
    try {
      if (provider) {
        await api.updateProvider(provider.id, v);
      } else {
        await api.addProvider(v);
      }
      form.resetFields();
      setKind("");
      onSaved();
    } catch (e) {
      message.error(t("config.operationFailed", { operation: t(provider ? "config.operation.update" : "config.operation.add"), detail: (e as Error).message }));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal
      className="provider-config-modal"
      width={860}
      centered
      title={t("config.title", { operation: t(provider ? "config.edit" : "config.add"), provider: providerLabel })}
      open={open}
      onCancel={() => {
        form.resetFields();
        setKind("");
        onClose();
      }}
      onOk={submit}
      confirmLoading={submitting || (editing && !editReady && !loadError)}
      okButtonProps={{ disabled: !editReady || loadError }}
      okText={t(provider ? "config.save" : "config.add")}
      cancelText={t("config.cancel")}
      destroyOnHidden
    >
      {loadError && <Alert type="error" showIcon message={t("config.loadFailed")} />}
      <Form
        disabled={!editReady || loadError}
        className="provider-config-form"
        form={form}
        layout="horizontal"
        labelAlign="left"
        colon={false}
        labelCol={{ flex: "140px" }}
        wrapperCol={{ flex: "auto" }}
      >
        <div className="provider-config-panel">
          <div className="provider-modal-section-title">{t("config.basic")}</div>
          {provider ? (
            <Form.Item label={t("config.type")}>
              <Input aria-label={t("config.type")} value={t(`kind.${provider.kind}`, { defaultValue: provider.kind })} disabled />
            </Form.Item>
          ) : (
            <Form.Item label={t("config.type")} name="kind" rules={[{ required: true, message: t("config.typeRequired") }]}>
              <Select
                value={kind || undefined}
                onChange={(v) => {
                  setKind(v);
                  form.setFieldValue("kind", v);
                  form.setFieldsValue({
                    endpoint: undefined,
                    api_key: undefined,
                    public_key: undefined,
                    private_key: undefined,
                    extra: {},
                    ...credentialDefaults(kindInfos.find((info) => info.kind === v)),
                  });
                }}
                options={kindInfos.map((k) => ({ value: k.kind, label: t(`kind.${k.kind}`, { defaultValue: localizedText(k.label, language) || k.kind }) }))}
              />
            </Form.Item>
          )}
          {requirement && (
            <Form.Item>
              <Alert type="info" showIcon message={requirement} />
            </Form.Item>
          )}
          <Form.Item label={t("config.name")} name="name" rules={[{ required: true, whitespace: true, message: t("config.nameRequired") }]}>
            <Input placeholder={t("config.namePlaceholder", { kind: selectedKindLabel })} />
          </Form.Item>
          <div className="provider-modal-section-title provider-modal-credential-title">{t("config.credentials")}</div>
          {(selectedKind?.credential_fields ?? []).map((field) => (
            <CredentialFieldInput key={`${field.extra ? "extra" : "base"}:${field.name}`} field={field} editing={editing} />
          ))}
          {(provider?.kind ?? kind) === "autodl" && (
            // AutoDL's API lists Pro container instances only; other instances
            // never appear, which would otherwise look like a broken key.
            <div className="provider-credential-hint">{t("config.autodlProOnly")}</div>
          )}
          {(provider?.kind ?? kind) === "runpod" && (
            // Only Secure Cloud pods are supported; say so before a Community
            // Cloud pod fails to install.
            <div className="provider-credential-hint">{t("config.runpodSecureOnly")}</div>
          )}
          {editing && (
            <div className="provider-credential-hint">
              {t("config.credentialsRetained")}
            </div>
          )}
        </div>
      </Form>
    </Modal>
  );
}

function CredentialFieldInput({ field, editing }: { field: ProviderCredentialField; editing: boolean }) {
  const { t, i18n } = useTranslation("providers");
  const language = i18n.resolvedLanguage || i18n.language;
  const label = localizedText(field.label, language) || field.name;
  const placeholder = localizedText(field.placeholder, language) || undefined;
  const input = field.secret ? (
    <Input.Password placeholder={placeholder} autoComplete="off" />
  ) : (
    <Input placeholder={placeholder} />
  );
  return (
    <Form.Item
      label={label}
      name={field.extra ? ["extra", field.name] : field.name}
      preserve={false}
      rules={[
        { required: !editing && field.required, message: t("config.fieldRequired", { field: label }) },
        {
          validator: async (_, value: unknown) => {
            if (field.default_template !== undefined && typeof value === "string" && value.trim() === field.default_template.trim()) {
              throw new Error(t("config.templateUnchanged", { field: label }));
            }
          },
        },
      ]}
    >
      {input}
    </Form.Item>
  );
}

function credentialDefaults(info?: ProviderKindInfo): Record<string, unknown> {
  const values: Record<string, unknown> = {};
  const extra: Record<string, string> = {};
  for (const field of info?.credential_fields ?? []) {
    if (field.secret) continue;
    const value = field.default_template ?? field.default_value;
    if (value === undefined) continue;
    if (field.extra) extra[field.name] = value;
    else values[field.name] = value;
  }
  return { ...values, extra };
}
