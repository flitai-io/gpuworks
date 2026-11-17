import { supportsChatInput, unsupportedChatInput } from "./inputModalities";
import { historyMessages } from "./conversations/history";
import { userContent } from "./api";
import {
  AudioOutlined,
  CloseOutlined,
  DeleteOutlined,
  FileImageOutlined,
  LoadingOutlined,
  PlusOutlined,
  SendOutlined,
} from "@ant-design/icons";
import { Alert, Button, Checkbox, Input, Popconfirm, Segmented, Skeleton, Space, Tag, Tooltip, Typography, message } from "antd";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { api as platformAPI } from "../../api";
import { usePlaygroundPriorityClasses } from "./priorityClasses";
import ChatPane from "./ChatPane";
import ConversationSidebar from "./ConversationSidebar";
import { isEmpty, MAX_COLUMNS, type ConversationMode } from "./conversations/model";
import type { SendResult } from "./conversations/store";
import { useConversations } from "./conversations/useConversations";
import { HtmlPreviewProvider } from "./HtmlPreview";
import { initialParameterState } from "./ParametersPanel";
import { HTTP1_STREAM_LIMIT, MULTIPLEXED_STREAM_LIMIT } from "./streamLimit";
import type { Attachment, PlaygroundCatalog } from "./types";
import { useTranslation } from "react-i18next";

const COLLAPSED_KEY = "gpuworks.playground.sidebar-collapsed";

// The list starts collapsed on narrow screens, where it would cover the chat.
function readCollapsed(): boolean {
  try {
    const saved = localStorage.getItem(COLLAPSED_KEY);
    if (saved) return saved === "1";
  } catch {
    // Blocked storage only loses the preference.
  }
  return typeof window !== "undefined" && window.innerWidth <= 768;
}

export default function ChatPage({ catalog, onCatalogChanged }: { catalog: PlaygroundCatalog; onCatalogChanged: () => void }) {
  const { t } = useTranslation("playground");
  const [store, state] = useConversations();
  const priorityClasses = usePlaygroundPriorityClasses();
  const [searchParams, setSearchParams] = useSearchParams();
  const [prompt, setPrompt] = useState("");
  const [attachments, setAttachments] = useState<Attachment[]>([]);
  const [limited, setLimited] = useState<Extract<SendResult, { status: "limited" }>>();
  const [pendingMode, setPendingMode] = useState<ConversationMode>();
  const [collapsed, setCollapsed] = useState(readCollapsed);
  // One follow state for the whole page: scrolling up in any column means
  // the user is reading, so every column stops jumping to the newest token.
  const [follow, setFollow] = useState(true);
  const stopFollowing = useCallback(() => setFollow(false), []);
  const inputRef = useRef<HTMLTextAreaElement | null>(null);
  const handledModel = useRef<string>(undefined);
  const queryModel = searchParams.get("model") || undefined;
  const streamOptions = useMemo(() => catalog.stream_options || [], [catalog.stream_options]);
  const chatModels = useMemo(() => catalog.models.filter((model) => model.operations.some((operation) => operation.type === "openai.chat")), [catalog.models]);
  const current = state.conversations.find((conversation) => conversation.id === state.currentID);
  const firstModel = current?.columns[0]?.model;
  const modelsAvailable = !!current?.columns.every((column) => chatModels.some((model) => model.id === column.model));

  useEffect(() => {
    let alive = true;
    platformAPI.me()
      .then((user) => { if (alive) void store.start(user.id); })
      .catch(() => { if (alive) void store.start(0); });
    return () => { alive = false; };
  }, [store]);
  useEffect(() => {
    store.ensureConversation({ model: queryModel || chatModels[0]?.id || "", parameters: initialParameterState(catalog.parameters, streamOptions) });
  }, [store, state.status, state.currentID, queryModel, chatModels, catalog.parameters, streamOptions]);
  // ?model= names the model to show (a link from a deployment); afterwards
  // the URL follows the first column of the open conversation.
  useEffect(() => {
    if (state.status !== "ready" || !current) return;
    if (queryModel && queryModel !== handledModel.current) {
      handledModel.current = queryModel;
      store.openModel(queryModel);
      return;
    }
    if (firstModel && firstModel !== queryModel) {
      handledModel.current = firstModel;
      const next = new URLSearchParams(searchParams);
      next.set("model", firstModel);
      setSearchParams(next, { replace: true });
    }
  }, [state.status, current, firstModel, queryModel, searchParams, setSearchParams, store]);

  const collapse = (next: boolean) => {
    setCollapsed(next);
    try { localStorage.setItem(COLLAPSED_KEY, next ? "1" : "0"); } catch { /* only the preference is lost */ }
  };
  const supportsInput = (modality: string) => !!current?.columns.every((column) =>
    supportsChatInput(chatModels.find((model) => model.id === column.model), modality));
  const inputWarning = (modelID: string, modality: string) => {
    const model = chatModels.find((entry) => entry.id === modelID);
    return t("chat.unsupportedInput", { model: model?.label || modelID, modality,
      supported: model?.input_modalities?.join(", ") || "?" });
  };
  const disabledInputReason = (modality: string) => {
    const column = current?.columns.find((item) => !supportsChatInput(chatModels.find((model) => model.id === item.model), modality));
    return column ? inputWarning(column.model, modality) : undefined;
  };
  const submit = () => {
    if (!prompt.trim() && !attachments.length) return;
    if (!modelsAvailable) { message.warning(t("chat.selectAvailableModel")); return; }
    for (const column of current?.columns || []) {
      const modality = unsupportedChatInput(chatModels.find((model) => model.id === column.model), [
        ...historyMessages(column), { role: "user", content: userContent(prompt, attachments) },
      ]);
      if (modality) { message.warning(inputWarning(column.model, modality)); return; }
    }
    const result = store.send(prompt, attachments, streamOptions);
    if (result.status === "limited") { setLimited(result); return; }
    if (result.status === "busy") { message.warning(t("chat.busyElsewhere")); return; }
    if (result.status === "invalid") { if (result.error) message.error(result.error); return; }
    setLimited(undefined);
    setPrompt("");
    setAttachments([]);
    // A new question means the user is waiting for its answer.
    setFollow(true);
    inputRef.current?.focus();
  };
  const addFiles = async (files: FileList | null, type: "image" | "audio") => {
    if (!files?.length) return;
    if (!supportsInput(type)) {
      const column = current?.columns.find((item) => !supportsChatInput(chatModels.find((model) => model.id === item.model), type));
      if (column) message.warning(inputWarning(column.model, type));
      return;
    }
    try {
      const next = await Promise.all(Array.from(files).map(async (file) => ({
        id: `${Date.now()}-${file.name}-${Math.random()}`,
        name: file.name,
        type,
        dataURL: await readDataURL(file, t("chat.fileReadFailed", { name: file.name })),
      } satisfies Attachment)));
      setAttachments((items) => [...items, ...next].slice(0, 6));
    } catch (error) {
      message.error((error as Error).message);
    }
  };
  const switchMode = (mode: ConversationMode) => {
    if (!current || mode === current.mode) return;
    if (isEmpty(current)) store.setMode(mode);
    else setPendingMode(mode);
  };

  if (state.status !== "ready" || !current) {
    return <div className="playground-chat-page"><Skeleton active paragraph={{ rows: 6 }} /></div>;
  }
  const compare = current.mode === "compare";
  const selectedModels = current.columns.map((column) => column.model).filter(Boolean);
  const duplicateModels = compare && new Set(selectedModels).size < selectedModels.length;
  const columns = current.columns.length;
  const lastColumn = current.columns[columns - 1];
  return (
    <HtmlPreviewProvider allowNetwork={catalog.html_preview?.allow_network ?? false}>
      <div className="playground-chat-page">
        <ConversationSidebar
          store={store}
          conversations={state.conversations}
          currentID={state.currentID}
          persistent={state.persistent}
          models={catalog.models}
          collapsed={collapsed}
          onCollapse={collapse}
        />
        <div className="playground-chat-main">
          <div className="playground-chat-controls">
            <Space size={12} wrap>
              <Popconfirm
                open={!!pendingMode}
                title={t(pendingMode === "compare" ? "chat.toCompareConfirm" : "chat.toSingleConfirm")}
                okText={t("chat.newConversation")}
                cancelText={t("conversations.cancel")}
                onConfirm={() => { if (pendingMode) store.setMode(pendingMode); setPendingMode(undefined); }}
                onCancel={() => setPendingMode(undefined)}
              >
                <Segmented
                  value={current.mode}
                  onChange={(value) => switchMode(value as ConversationMode)}
                  options={[{ label: t("chat.single"), value: "single" }, { label: t("chat.compare"), value: "compare" }]}
                />
              </Popconfirm>
              {compare && (
                <Space size={4}>
                  <Typography.Text type="secondary">{t("chat.columns")}</Typography.Text>
                  <Tooltip title={columns >= MAX_COLUMNS ? t("chat.maxColumns") : t("chat.addColumn")}><Button icon={<PlusOutlined />} disabled={columns >= MAX_COLUMNS} onClick={() => store.addColumn()} aria-label={t("chat.addColumn")} /></Tooltip>
                  <Popconfirm
                    title={t("chat.removeColumnConfirm", { number: columns })}
                    disabled={columns <= 2 || !lastColumn.messages.length}
                    okText={t("chat.removeColumn")}
                    okButtonProps={{ danger: true }}
                    cancelText={t("conversations.cancel")}
                    onConfirm={() => store.removeColumn()}
                  >
                    <Tooltip title={columns <= 2 ? t("chat.minColumns") : t("chat.removeColumn")}>
                      <Button icon={<CloseOutlined />} disabled={columns <= 2} onClick={() => { if (!lastColumn.messages.length) store.removeColumn(); }} aria-label={t("chat.removeColumn")} />
                    </Tooltip>
                  </Popconfirm>
                </Space>
              )}
            </Space>
            <Space size={12}>
              <Checkbox checked={follow} onChange={(event) => setFollow(event.target.checked)}>{t("chat.followLatest")}</Checkbox>
              <Tooltip title={t("chat.clearTitle")}>
                <Button icon={<DeleteOutlined />} onClick={() => store.clear()}>{t("chat.clear")}</Button>
              </Tooltip>
            </Space>
          </div>
          <div className={`playground-chat-grid columns-${columns}`}>
            {current.columns.map((column, index) => (
              <ChatPane
                key={column.id}
                store={store}
                column={column}
                models={catalog.models}
                definitions={catalog.parameters}
                streamOptions={streamOptions}
                number={compare ? index + 1 : undefined}
                onCatalogChanged={onCatalogChanged}
                follow={follow}
                onScrollAway={stopFollowing}
                priorityClasses={priorityClasses}
              />
            ))}
          </div>
          {duplicateModels && (
            <Alert
              className="playground-limit-alert"
              type="warning"
              showIcon
              message={t("chat.sharedModelTitle")}
              description={t("chat.sharedModelDescription")}
            />
          )}
          {limited && (
            <Alert
              className="playground-limit-alert"
              type="warning"
              showIcon
              closable
              onClose={() => setLimited(undefined)}
              message={t("chat.limitTitle", { limit: limited.limit })}
              description={state.limit.multiplexed ? t("chat.limitWait") : t("chat.limitHttp1", { limit: HTTP1_STREAM_LIMIT, multiplexed: MULTIPLEXED_STREAM_LIMIT })}
            />
          )}
          <div className="playground-composer">
            {!!attachments.length && (
              <div className="playground-attachments">
                {attachments.map((attachment) => (
                  <Tag key={attachment.id} closable onClose={() => setAttachments((items) => items.filter((item) => item.id !== attachment.id))}>
                    {attachment.type === "image" ? t("chat.image") : t("chat.audio")} · {attachment.name}
                  </Tag>
                ))}
              </div>
            )}
            <Input.TextArea
              id="playground-chat-input"
              name="playground-chat-input"
              aria-label={t("chat.inputAria")}
              ref={(node) => { inputRef.current = node?.resizableTextArea?.textArea || null; }}
              autoSize={{ minRows: 2, maxRows: 6 }}
              value={prompt}
              placeholder={compare ? t("chat.comparePlaceholder", { count: columns }) : t("chat.placeholder")}
              onChange={(event) => setPrompt(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
                  event.preventDefault();
                  submit();
                }
              }}
            />
            <div className="playground-composer-actions">
              <Space size={2}>
                <FilePicker disabled={!supportsInput("image")} disabledReason={disabledInputReason("image")} accept="image/*" title={t("chat.addImage")} icon={<FileImageOutlined />} onFiles={(files) => addFiles(files, "image")} />
                <FilePicker disabled={!supportsInput("audio")} disabledReason={disabledInputReason("audio")} accept="audio/*" title={t("chat.addAudio")} icon={<AudioOutlined />} onFiles={(files) => addFiles(files, "audio")} />
              </Space>
              <Space size={10}>
                {!!state.running && (
                  <Tag className="playground-generating-count" color={state.running >= state.limit.limit ? "warning" : "processing"} icon={<LoadingOutlined />}>
                    {t("chat.generatingCount", { running: state.running, limit: state.limit.limit })}
                  </Tag>
                )}
                <Button type="primary" icon={<SendOutlined />} disabled={!modelsAvailable || (!prompt.trim() && !attachments.length)} onClick={submit}>
                  {t("chat.send")}
                </Button>
              </Space>
            </div>
          </div>
        </div>
      </div>
    </HtmlPreviewProvider>
  );
}

function FilePicker({ accept, title, icon, onFiles, disabled, disabledReason }: {
  disabled: boolean;
  disabledReason?: string;
  accept: string;
  title: string;
  icon: React.ReactNode;
  onFiles: (files: FileList | null) => void;
}) {
  const ref = useRef<HTMLInputElement>(null);
  return <>
    <input ref={ref} className="playground-hidden-input" type="file" disabled={disabled} accept={accept} multiple onChange={(event) => { onFiles(event.target.files); event.target.value = ""; }} />
    <Tooltip title={disabled ? disabledReason : title} trigger={["hover", "focus"]}>
      <span tabIndex={disabled ? 0 : undefined} role={disabled ? "group" : undefined} aria-label={disabled ? disabledReason : undefined}>
        <Button type="text" disabled={disabled} icon={icon} onClick={() => ref.current?.click()} aria-label={title} />
      </span>
    </Tooltip>
  </>;
}

function readDataURL(file: File, errorMessage: string): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result));
    reader.onerror = () => reject(new Error(errorMessage));
    reader.readAsDataURL(file);
  });
}
