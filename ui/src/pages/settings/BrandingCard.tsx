import SettingsSaveButton from "./SettingsSaveButton";
import { useEffect, useState } from "react";
import { Alert, Button, Card, Form, Input, Space, Typography, Upload, message } from "antd";
import { UploadOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { api, type Branding } from "../../api";
import { apiErrorText } from "../../apiError";
import { useIsAdmin } from "../../components/useIsAdmin";
import { BrandMark, DEFAULT_BRANDING, DEFAULT_PRODUCT_NAME, setBranding } from "../../branding";
import { Button as ZeronButton } from "../../components/ui/button";
import { ConfirmPopover } from "../../ui-kit/ConfirmPopover";
import LogoCropModal from "./LogoCropModal";

const { Text } = Typography;

/** BRANDING_ANCHOR is the fragment links to this card use (/settings#branding). */
export const BRANDING_ANCHOR = "branding";

// The server's limits; the inputs stop at them so a save cannot fail on length.
const PRODUCT_NAME_MAX = 32;
const CONSOLE_TITLE_MAX = 64;
const LOGIN_DESCRIPTION_MAX = 200;

export default function BrandingCard() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const isAdmin = useIsAdmin();
  const [draft, setDraft] = useState<Branding>();
  const [loadError, setLoadError] = useState("");
  const [saving, setSaving] = useState(false);
  // The picked image waiting in the crop dialog.
  const [picked, setPicked] = useState<File | null>(null);

  const load = () => api.branding()
    .then((value) => { setDraft(value); setLoadError(""); })
    .catch((error) => setLoadError(apiErrorText(tCommon, error)));
  useEffect(() => { void load(); }, []);

  const edit = (patch: Partial<Branding>) => setDraft((current) => current && { ...current, ...patch });
  const disabled = !isAdmin || !draft;

  const pickLogo = (file: File) => {
    if (file.type.startsWith("image/")) setPicked(file);
    else message.error(t("branding.logoError.notImage"));
  };

  const save = async () => {
    if (!draft) return;
    setSaving(true);
    try {
      const saved = await api.updateBranding(draft);
      setDraft(saved);
      setBranding(saved);
      message.success(t("branding.saved"));
    } catch (error) {
      message.error(t("error.save", { detail: apiErrorText(tCommon, error) }));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card
      id={BRANDING_ANCHOR}
      title={t("branding.title")}
      extra={<Space>
        <ConfirmPopover
          trigger={<ZeronButton variant="ghost" size="sm" disabled={disabled || saving || !Object.values(draft ?? {}).some(Boolean)}>{t("branding.restoreAll")}</ZeronButton>}
          title={t("branding.confirmRestoreAll")}
          onConfirm={() => setDraft(DEFAULT_BRANDING)}
        />
        <SettingsSaveButton loading={saving} disabled={disabled} onSave={save} />
      </Space>}
    >
      <Space direction="vertical" size={8} style={{ width: "100%", maxWidth: 720 }}>
        <Text type="secondary">{t("branding.description")}</Text>
        {loadError && <Alert type="error" showIcon message={loadError} action={<Button size="small" onClick={() => void load()}>{tCommon("actions.retry")}</Button>} />}
        {!isAdmin && <Alert type="info" showIcon message={t("branding.adminOnly")} />}
        <Form layout="vertical" style={{ marginTop: 8 }} disabled={disabled || saving}>
          <Form.Item label={t("branding.logo")} extra={t("branding.logoHelp")}>
            <Space size={12} wrap>
              <span className="branding-logo-preview"><BrandMark logo={draft?.logo ?? ""} /></span>
              <Upload
                accept="image/png,image/jpeg,image/webp,image/svg+xml"
                showUploadList={false}
                beforeUpload={(file) => { pickLogo(file); return false; }}
                disabled={disabled || saving}
              >
                <Button icon={<UploadOutlined />}>{t("branding.uploadLogo")}</Button>
              </Upload>
              {draft?.logo && <ConfirmPopover
                trigger={<ZeronButton variant="secondary" size="sm" disabled={disabled || saving}>{t("branding.restoreDefault")}</ZeronButton>}
                title={t("branding.confirmRestoreLogo")}
                onConfirm={() => edit({ logo: "" })}
              />}
            </Space>
          </Form.Item>
          <Form.Item label={t("branding.productName")} extra={t("branding.productNameHelp")}>
            <Input
              value={draft?.product_name ?? ""}
              placeholder={DEFAULT_PRODUCT_NAME}
              maxLength={PRODUCT_NAME_MAX}
              showCount
              allowClear
              onChange={(event) => edit({ product_name: event.target.value })}
            />
          </Form.Item>
          <Form.Item label={t("branding.consoleTitle")} extra={t("branding.consoleTitleHelp")}>
            <Input
              value={draft?.console_title ?? ""}
              placeholder={t("branding.consoleTitlePlaceholder")}
              maxLength={CONSOLE_TITLE_MAX}
              showCount
              allowClear
              onChange={(event) => edit({ console_title: event.target.value })}
            />
          </Form.Item>
          <Form.Item label={t("branding.loginDescription")} extra={t("branding.loginDescriptionHelp")} style={{ marginBottom: 0 }}>
            <Input.TextArea
              value={draft?.login_description ?? ""}
              placeholder={tCommon("branding.defaultLoginDescription")}
              maxLength={LOGIN_DESCRIPTION_MAX}
              showCount
              autoSize={{ minRows: 2, maxRows: 5 }}
              onChange={(event) => edit({ login_description: event.target.value })}
            />
          </Form.Item>
        </Form>
      </Space>
      <LogoCropModal
        file={picked}
        onCancel={() => setPicked(null)}
        onConfirm={(logo) => { edit({ logo }); setPicked(null); }}
      />
    </Card>
  );
}
