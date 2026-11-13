import { Alert, Form, Input, Modal, Typography } from "antd";
import { useState } from "react";
import { useTranslation } from "react-i18next";

type Props = {
  imageRef: string;
  nodeLabel: string;
  onCancel: () => void;
  onStart: (path: string) => Promise<void>;
};

export default function ImageArchivePathDialog({ imageRef, nodeLabel, onCancel, onStart }: Props) {
  const { t } = useTranslation("images");
  const [path, setPath] = useState("");
  const [starting, setStarting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const submit = async () => {
    if (!path.trim() || starting) return;
    setStarting(true);
    setError(null);
    try {
      await onStart(path.trim());
    } catch (e) {
      setError(e instanceof Error ? e.message : t("action.pullFailed"));
    } finally {
      setStarting(false);
    }
  };
  return (
    <Modal
      open
      title={t("nodeTable.localDialog.title", { node: nodeLabel })}
      onCancel={onCancel}
      onOk={() => void submit()}
      okText={t("nodeTable.localDialog.start")}
      cancelText={t("common.cancel")}
      confirmLoading={starting}
      okButtonProps={{ disabled: !path.trim(), "aria-label": t("nodeTable.localDialog.start") }}
      cancelButtonProps={{ disabled: starting }}
      closable={!starting}
      maskClosable={!starting}
      keyboard={!starting}
    >
      <Typography.Paragraph>{t("nodeTable.localDialog.description", { ref: imageRef })}</Typography.Paragraph>
      <Form layout="vertical" component="div">
        <Form.Item label={t("nodeTable.localDialog.path")} htmlFor="image-archive-path" extra={t("nodeTable.localDialog.help")}>
          <Input
            id="image-archive-path"
            value={path}
            disabled={starting}
            placeholder="/data/images/runner.tar"
            onChange={(event) => { setPath(event.target.value); setError(null); }}
            onPressEnter={() => void submit()}
          />
        </Form.Item>
      </Form>
      {error ? <Alert type="error" showIcon message={error} /> : null}
    </Modal>
  );
}
