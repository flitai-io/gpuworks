import UnsupportedModel from "./UnsupportedModel";
import SleepNotice from "./SleepNotice";
import {
  ClearOutlined,
  CodeOutlined,
  ControlOutlined,
  PauseCircleOutlined,
} from "@ant-design/icons";
import { Button, Empty, Select, Space, Tooltip, message } from "antd";
import { memo, useEffect, useMemo, useRef, useState } from "react";
import { api as platformAPI } from "../../api";
import { knownClass, type PlaygroundPriorityClasses } from "./priorityClasses";
import { useFollowScroll } from "../../hooks/useFollowScroll";
import { buildChatBody } from "./api";
import { historyMessages } from "./conversations/history";
import type { ConversationColumn } from "./conversations/model";
import type { ConversationStore } from "./conversations/store";
import MessageBubble from "./MessageBubble";
import ParametersPanel from "./ParametersPanel";
import type {
  ChatMessage,
  ParameterDefinition,
  PlaygroundModel,
  StreamOptionDefinition,
} from "./types";
import ViewCodeModal from "./ViewCodeModal";
import { useTranslation } from "react-i18next";

interface ChatPaneProps {
  store: ConversationStore;
  column: ConversationColumn;
  models: PlaygroundModel[];
  definitions: ParameterDefinition[];
  streamOptions: StreamOptionDefinition[];
  /** The column number in a compare conversation. */
  number?: number;
  onCatalogChanged?: () => void;
  /** Keep the message list pinned to the newest reply. */
  follow: boolean;
  /** The user scrolled away from the newest reply. */
  onScrollAway: () => void;
  /** The request priority classes a reply may ask for. */
  priorityClasses: PlaygroundPriorityClasses;
}

type Entry = { kind: "message"; message: ChatMessage } | { kind: "switch"; key: string; model: string };

/**
 * entries interleaves the history with a marker wherever the column's model
 * changed: before the question the new model answered first, and at the end
 * when the model was switched after the last reply.
 */
function entries(column: ConversationColumn): Entry[] {
  const out: Entry[] = [];
  let lastModel: string | undefined;
  column.messages.forEach((message, index) => {
    const reply = message.role === "user" ? column.messages[index + 1] : undefined;
    if (reply?.model && lastModel && reply.model !== lastModel) out.push({ kind: "switch", key: `switch-${message.id}`, model: reply.model });
    if (message.role === "assistant" && message.model) lastModel = message.model;
    out.push({ kind: "message", message });
  });
  if (lastModel && column.model && column.model !== lastModel) out.push({ kind: "switch", key: "switch-pending", model: column.model });
  return out;
}

// Memoized: a reply streaming in one column leaves the other columns alone.
const ChatPane = memo(function ChatPane({
  store,
  column,
  models,
  definitions,
  streamOptions,
  number,
  onCatalogChanged,
  follow,
  onScrollAway,
  priorityClasses,
}: ChatPaneProps) {
  const { t } = useTranslation("playground");
  const chatModels = useMemo(() => models.filter((model) => model.operations.some((operation) => operation.type === "openai.chat")), [models]);
  const [showParameters, setShowParameters] = useState(false);
  // A class saved with the conversation that no longer exists falls back to
  // the default, or every reply would be refused.
  const chosenClass = column.parameters.priorityClass;
  const keptClass = knownClass(chosenClass, priorityClasses);
  useEffect(() => {
    if (keptClass !== chosenClass) store.setParameters(column.id, { ...column.parameters, priorityClass: keptClass });
  }, [keptClass, chosenClass, store, column.id, column.parameters]);
  const [codeOpen, setCodeOpen] = useState(false);
  const [waking, setWaking] = useState(false);
  const viewportRef = useRef<HTMLDivElement>(null);
  const messages = column.messages;
  useFollowScroll(viewportRef, follow, onScrollAway, messages);

  const selectedModel = chatModels.find((model) => model.id === column.model);
  const unavailableOption = column.model && !selectedModel
    ? [{ value: column.model, label: t(models.some((model) => model.id === column.model) ? "pane.unsupportedSelection" : "pane.unavailableSelection", { model: column.model }), disabled: true }]
    : [];
  const codeBody = useMemo(() => {
    const history = historyMessages(column);
    try {
      return buildChatBody(column.model, history, column.parameters, streamOptions);
    } catch {
      return { model: column.model, messages: history, stream: true };
    }
  }, [column, streamOptions]);
  const wake = async () => {
    if (!selectedModel?.deployment_id) return;
    setWaking(true);
    try {
      await platformAPI.wakeDeployment(selectedModel.deployment_id);
      message.success(t("pane.wakeSubmitted"));
      onCatalogChanged?.();
    } catch (error) {
      message.error((error as Error).message);
    } finally {
      setWaking(false);
    }
  };

  return (
    <section className="playground-chat-pane">
      <header className="playground-pane-toolbar">
        {number && <span className="playground-column-number">{number}</span>}
        <Select
          className="playground-model-select"
          aria-label={number ? t("pane.columnModel", { number }) : t("pane.model")}
          value={column.model || undefined}
          placeholder={t("pane.modelPlaceholder")}
          onChange={(value) => store.setModel(column.id, value)}
          options={[...unavailableOption, ...chatModels.map((model) => ({ value: model.id, label: model.label }))]}
          showSearch
          optionFilterProp="label"
        />
        <Space size={2}>
          <Tooltip title={t("pane.parameters")}><Button type={showParameters ? "primary" : "text"} icon={<ControlOutlined />} onClick={() => setShowParameters((value) => !value)} aria-label={t("pane.parameters")} /></Tooltip>
          <Tooltip title={t("pane.viewCode")}><Button type="text" icon={<CodeOutlined />} onClick={() => setCodeOpen(true)} disabled={!selectedModel} aria-label={t("pane.viewCode")} /></Tooltip>
          <Tooltip title={t("pane.clear")}><Button type="text" icon={<ClearOutlined />} onClick={() => store.clear(column.id)} aria-label={t("pane.clear")} /></Tooltip>
        </Space>
      </header>
      <SleepNotice className="playground-sleep-alert" model={selectedModel} action={<Button size="small" loading={waking} onClick={wake}>{t("pane.wake")}</Button>} />
      <div className={`playground-pane-body${showParameters ? " has-parameters" : ""}`}>
        <div className="playground-message-list" ref={viewportRef}>
          {!messages.length && (
            <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={chatModels.length ? t("pane.start") : t("pane.noModels")} />
          )}
          {entries(column).map((entry) => entry.kind === "message"
            ? <MessageBubble key={entry.message.id} message={entry.message} />
            : <div key={entry.key} className="playground-model-switch" role="separator"><span>{t("pane.modelSwitched", { model: entry.model })}</span></div>)}
        </div>
        {showParameters && (
          <aside className="playground-parameter-aside">
            {number && <Button size="small" block onClick={() => { store.applyParameters(column.id); message.success(t("chat.appliedAll")); }}>{t("pane.applyAll")}</Button>}
            <ParametersPanel
              definitions={definitions}
              presets={selectedModel?.extra_parameter_presets ?? []}
              streamOptions={streamOptions}
              value={column.parameters}
              onChange={(value) => store.setParameters(column.id, value)}
              priority={selectedModel?.request_priority ? { classes: priorityClasses.classes, scheduling: !!selectedModel.priority_scheduling } : undefined}
            />
          </aside>
        )}
      </div>
      {store.generating(column) && (
        <Button className="playground-stop-button" icon={<PauseCircleOutlined />} onClick={() => store.stop(column.id)}>{t("pane.stop")}</Button>
      )}
      {column.model && !selectedModel && <UnsupportedModel modelID={column.model} models={models} />}
      {selectedModel && <ViewCodeModal open={codeOpen} onClose={() => setCodeOpen(false)} body={codeBody} model={selectedModel} />}
    </section>
  );
});

export default ChatPane;
