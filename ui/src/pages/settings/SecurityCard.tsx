import { useCallback, useEffect, useRef, useState } from "react";
import { Alert, Button, Card, Form, Space, Switch, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type SecuritySettings } from "../../api";
import { apiErrorText } from "../../apiError";
import { useIsAdmin } from "../../components/useIsAdmin";

const { Text } = Typography;

/** SECURITY_ANCHOR is the fragment the playground links to (/settings#security). */
export const SECURITY_ANCHOR = "security";

/** How long a deep link keeps the card in view while the cards above it load. */
const SETTLE_MS = 3000;

export default function SecurityCard() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const isAdmin = useIsAdmin();
  const [previewNetwork, setPreviewNetwork] = useState<boolean>();
  const [openAccess, setOpenAccess] = useState<boolean>();
  // The policy as last read from (or written to) the server. Comparing against
  // it keeps a save to the switches the administrator actually moved, so the
  // partial PATCH never carries a stale value for the other one.
  const [stored, setStored] = useState<SecuritySettings>();
  const [loadError, setLoadError] = useState("");
  const [saving, setSaving] = useState(false);
  const cardRef = useRef<HTMLDivElement>(null);

  const load = useCallback(() => api.securitySettings()
    .then((settings) => {
      setPreviewNetwork(settings.playground_preview_network);
      setOpenAccess(settings.inference_open_access);
      setStored(settings);
      setLoadError("");
    })
    .catch((error) => setLoadError(apiErrorText(tCommon, error))), [tCommon]);

  useEffect(() => { void load(); }, [load]);
  // The card is rendered after navigation, so the browser's own jump to the
  // fragment happened against an empty page; and the cards above it load
  // their own data and grow afterwards. Follow the card while the page
  // settles, until the user scrolls on their own.
  useEffect(() => {
    if (window.location.hash !== `#${SECURITY_ANCHOR}` || typeof ResizeObserver === "undefined") return;
    const card = cardRef.current;
    if (!card) return;
    const jump = () => card.scrollIntoView?.({ block: "start" });
    const observer = new ResizeObserver(jump);
    const stop = () => {
      observer.disconnect();
      window.removeEventListener("wheel", stop);
      window.removeEventListener("touchmove", stop);
      window.removeEventListener("keydown", stop);
    };
    jump();
    observer.observe(document.body);
    window.addEventListener("wheel", stop, { passive: true });
    window.addEventListener("touchmove", stop, { passive: true });
    window.addEventListener("keydown", stop);
    const timer = window.setTimeout(stop, SETTLE_MS);
    return () => {
      window.clearTimeout(timer);
      stop();
    };
  }, []);

  // The backend patches only the fields it is given; send exactly the ones the
  // administrator changed since the last load or save.
  const pending: Partial<SecuritySettings> = {};
  if (stored) {
    if (previewNetwork !== undefined && previewNetwork !== stored.playground_preview_network) {
      pending.playground_preview_network = previewNetwork;
    }
    if (openAccess !== undefined && openAccess !== stored.inference_open_access) {
      pending.inference_open_access = openAccess;
    }
  }
  const dirty = Object.keys(pending).length > 0;

  const save = async () => {
    if (!dirty) return;
    setSaving(true);
    try {
      const saved = await api.updateSecuritySettings(pending);
      setPreviewNetwork(saved.playground_preview_network);
      setOpenAccess(saved.inference_open_access);
      setStored(saved);
      message.success(t("security.saved"));
    } catch (error) {
      message.error(t("error.save", { detail: apiErrorText(tCommon, error) }));
    } finally {
      setSaving(false);
    }
  };

  const loaded = previewNetwork !== undefined && openAccess !== undefined;
  return (
    <div id={SECURITY_ANCHOR} ref={cardRef}>
      <Card
        title={t("security.title")}
        extra={<Button type="primary" loading={saving} disabled={!isAdmin || !loaded || !dirty} onClick={() => void save()}>{t("common.save")}</Button>}
      >
        <Space direction="vertical" size={8} style={{ width: "100%", maxWidth: 720 }}>
          <Text type="secondary">{t("security.description")}</Text>
          {loadError && <Alert type="error" showIcon message={loadError} action={<Button size="small" onClick={() => void load()}>{tCommon("actions.retry")}</Button>} />}
          {!isAdmin && <Alert type="info" showIcon message={t("security.adminOnly")} />}
          <Form layout="vertical" style={{ marginTop: 8 }}>
            <Form.Item label={t("security.previewNetwork")} extra={t("security.previewNetworkHelp")} style={{ marginBottom: 0 }}>
              <Switch
                checked={previewNetwork ?? false}
                loading={!loaded && !loadError}
                disabled={!isAdmin || !loaded}
                onChange={setPreviewNetwork}
                aria-label={t("security.previewNetwork")}
              />
            </Form.Item>
            <Form.Item label={t("security.inferenceOpenAccess")} extra={t("security.inferenceOpenAccessHelp")} style={{ marginTop: 8, marginBottom: 0 }}>
              <Switch
                checked={openAccess ?? false}
                loading={!loaded && !loadError}
                disabled={!isAdmin || !loaded}
                onChange={setOpenAccess}
                aria-label={t("security.inferenceOpenAccess")}
              />
            </Form.Item>
          </Form>
        </Space>
      </Card>
    </div>
  );
}
