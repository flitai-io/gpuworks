import { AudioOutlined, EditOutlined, FileImageOutlined, MessageOutlined, ReloadOutlined, ThunderboltOutlined, VideoCameraOutlined } from "@ant-design/icons";
import { Alert, Button, Segmented, Skeleton, Space, Tag, Typography } from "antd";
import { useCallback, useEffect, useState } from "react";
import { Navigate, useLocation, useNavigate, useSearchParams } from "react-router-dom";
import PageHeader from "../../components/PageHeader";
import { getPlaygroundCatalog } from "./api";
import AudioPage from "./AudioPage";
import ChatPage from "./ChatPage";
import CompletionPage from "./CompletionPage";
import ImagePage from "./ImagePage";
import VectorPage from "./VectorPage";
import VideoPage from "./VideoPage";
import RealtimePage from "./RealtimePage";
import DecisionPage from "./DecisionPage";
import type { PlaygroundCatalog } from "./types";
import { ConsoleIcon } from "../../ui-kit/consoleIcons";
import { useTranslation } from "react-i18next";

export default function PlaygroundPage() {
  const { t } = useTranslation("playground");
  const location = useLocation();
  const navigate = useNavigate();
  const [search, setSearch] = useSearchParams();
  const [catalog, setCatalog] = useState<PlaygroundCatalog>();
  const [error, setError] = useState<string>();
  const [loading, setLoading] = useState(true);
  const requestedMode = location.pathname.split("/").filter(Boolean).pop();
  const mode = (["chat", "completion", "audio", "vector", "image", "video", "realtime", "decision"].includes(requestedMode || "") ? requestedMode : "chat") as "chat" | "completion" | "audio" | "vector" | "image" | "video" | "realtime" | "decision";
  const load = useCallback(async () => {
    setLoading(true);
    try {
      setCatalog(await getPlaygroundCatalog());
      setError(undefined);
    } catch (next) {
      setError((next as Error).message);
    } finally {
      setLoading(false);
    }
  }, []);
  useEffect(() => { void load(); }, [load]);
  useEffect(() => {
    if (!catalog || search.get("model")) return;
    const templateID = Number(search.get("template_id") || 0);
    const specSource = search.get("spec_source") || "";
    const specName = search.get("spec") || "";
    if (!templateID) return;
    const matched = catalog.models.find((model) => model.template_id === templateID && (!specSource || model.template_spec_source === specSource) && (!specName || model.template_spec_name === specName));
    if (!matched) return;
    const next = new URLSearchParams(search); next.set("model", matched.id); setSearch(next, { replace: true });
  }, [catalog, search, setSearch]);

  if (location.pathname === "/playground") return <Navigate to="/playground/chat" replace />;
  return (
    <div className={mode === "chat" ? "playground-page playground-page--fit" : "playground-page"}>
      <PageHeader
        title={t("page.title")}
        description={t("page.description")}
        extra={<Space><Tag color="blue">{t("page.modelCount", { count: catalog?.models.length ?? 0 })}</Tag><Button icon={<ReloadOutlined />} loading={loading} onClick={() => void load()} aria-label={t("page.refresh")} /></Space>}
        toolbar={
          <>
            <Segmented
              className="playground-mode"
              value={mode}
              options={[
                { value: "chat", label: <span><MessageOutlined /> {t("page.chat")}</span> },
                { value: "completion", label: <span><EditOutlined /> {t("page.completion")}</span> },
                { value: "audio", label: <span><AudioOutlined /> {t("page.audio")}</span> },
                { value: "vector", label: <span># {t("page.vector")}</span> },
                { value: "image", label: <span><FileImageOutlined /> {t("page.image")}</span> },
                { value: "video", label: <span><VideoCameraOutlined /> {t("page.video")}</span> },
                { value: "realtime", label: <span><ThunderboltOutlined /> {t("page.realtime")}</span> },
                { value: "decision", label: <span><ConsoleIcon.decision size={14} className="inline-block align-[-0.125em]" /> {t("page.decision")}</span> },
              ]}
              onChange={(value) => navigate(`/playground/${value}`)}
            />
            <Typography.Text type="secondary">{t("page.parameterNotice")}</Typography.Text>
          </>
        }
      />
      {error && <Alert type="error" showIcon message={t("page.loadFailed")} description={error} action={<Button onClick={() => void load()}>{t("page.retry")}</Button>} />}
      {!catalog && loading ? <Skeleton active paragraph={{ rows: 8 }} /> : catalog && ({
        chat: <ChatPage catalog={catalog} onCatalogChanged={load} />,
        completion: <CompletionPage catalog={catalog} />,
        audio: <AudioPage catalog={catalog} />,
        vector: <VectorPage catalog={catalog} />,
        image: <ImagePage catalog={catalog} />,
        video: <VideoPage catalog={catalog} />,
        realtime: <RealtimePage catalog={catalog} />,
        decision: <DecisionPage catalog={catalog} />,
      }[mode])}
    </div>
  );
}
