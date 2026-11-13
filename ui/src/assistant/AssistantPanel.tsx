import { useEffect, useRef, useState, type ClipboardEvent, type DragEvent } from "react";
import { Alert, Button, Drawer, Empty, Input, Space, Spin, Tag, Tooltip, Typography } from "antd";
import { CloseOutlined, PlusOutlined, RobotOutlined, SendOutlined, WarningOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import MarkdownContent from "../components/MarkdownContent";
import { AnswerSummary, FindingCards, FitTable, ReasoningTail, StepList } from "../components/AIAssistantParts";
import { Button as ZButton } from "@/components/ui/button";
import { ConsoleIcon } from "../ui-kit/consoleIcons";
import { sameSubject, useAssistant, type AssistantSubject, type AssistantTask, type PageSubject, type Turn } from "./AssistantContext";
import { IMAGE_TYPES, ImagePrepError, prepareImage } from "./images";
import type { Thread } from "./threads";

const { Text } = Typography;

// Follow-ups each task offers after an answer: what users most often ask
// next, so the next step is a click too.
const followUps: Record<AssistantTask, string[]> = {
  "instance.diagnose": ["fullCommand", "compareHealthy", "withoutChange"],
  "model.fit": ["recommendConfig", "quantized", "longContext"],
  "deployment.unscheduled": ["closestNode", "lowerTP", "freeCards"],
  "node.health": ["confirmHardware", "acceptNew", "thermals"],
  ask: [],
};

// AssistantPanel is the assistant's resident side panel. It does not block
// the page: a user keeps working while an answer streams. Empty, it offers
// the tasks the current page's resources support, leading with what needs
// attention; in a thread, each answer ends with follow-up suggestions and
// the box below continues the thread.
export default function AssistantPanel() {
  const { t } = useTranslation("ai");
  const assistant = useAssistant();
  const { open, setOpen, turns, pageSubjects, imageLimit } = assistant;
  const [draft, setDraft] = useState("");
  const [images, setImages] = useState<string[]>([]);
  const [imageNote, setImageNote] = useState("");
  const [showThreads, setShowThreads] = useState(false);
  const fileInput = useRef<HTMLInputElement>(null);
  const [picked, setPicked] = useState<AssistantSubject | null | undefined>(undefined);
  const streaming = turns.some((turn) => turn.answer.status === "streaming");
  // The subject a free question is about: the user's pick, else the page's first.
  const about = picked === undefined ? pageSubjects[0]?.subject : picked ?? undefined;
  const thread = turns[0];

  // Other right-side drawers (a deployment's or node's detail) step aside
  // while the panel is open, so the answer and what it is about show side by
  // side (styles.css, .assistant-open).
  useEffect(() => {
    document.body.classList.toggle("assistant-open", open);
    return () => document.body.classList.remove("assistant-open");
  }, [open]);

  const send = (question: string, withImages: string[] = []) => {
    if ((!question.trim() && !withImages.length) || streaming) return;
    assistant.ask(question, about, withImages);
    setDraft("");
    setImages([]);
    setImageNote("");
  };

  // attach shrinks and adds images, within the service's per-message limit;
  // a service that takes none says so instead.
  const attach = async (files: File[]) => {
    const pictures = files.filter((file) => file.type.startsWith("image/"));
    if (!pictures.length) return;
    if (!imageLimit) {
      setImageNote(t("panel.imagesOff"));
      return;
    }
    const room = imageLimit.per_message - images.length;
    const notes: string[] = [];
    if (pictures.length > room) notes.push(t("panel.imagesTooMany", { count: imageLimit.per_message }));
    const added: string[] = [];
    for (const file of pictures.slice(0, Math.max(0, room))) {
      try {
        added.push(await prepareImage(file, imageLimit));
      } catch (e) {
        const reason = e instanceof ImagePrepError ? e.reason : "format";
        notes.push(t(`panel.image_${reason}`, { name: file.name, mb: Math.floor(imageLimit.max_bytes / (1 << 20)) }));
      }
    }
    setImages((current) => [...current, ...added].slice(0, imageLimit.per_message));
    setImageNote(notes.join(" "));
  };
  const onPaste = (event: ClipboardEvent) => {
    const files = [...event.clipboardData.files].filter((file) => file.type.startsWith("image/"));
    if (!files.length) return;
    event.preventDefault();
    void attach(files);
  };
  const onDrop = (event: DragEvent) => {
    const files = [...event.dataTransfer.files];
    if (!files.some((file) => file.type.startsWith("image/"))) return;
    event.preventDefault();
    void attach(files);
  };

  return (
    <Drawer
      open={open}
      onClose={() => setOpen(false)}
      mask={false}
      width="var(--assistant-panel-width)"
      closable={false}
      rootClassName="assistant-panel"
      styles={{ body: { padding: 0, display: "flex", flexDirection: "column" } }}
      title={
        <Space>
          <RobotOutlined />
          <span>{t("title")}</span>
          <Text type="secondary" style={{ fontSize: 12, fontWeight: 400 }}>{t("panel.shortcut")}</Text>
        </Space>
      }
      extra={
        <Space size={4}>
          {streaming && <Button size="small" onClick={assistant.stop}>{t("stop")}</Button>}
          <ZButton variant="ghost" size="sm" iconOnly active={showThreads} title={t("panel.history")} onClick={() => setShowThreads((v) => !v)} aria-label={t("panel.history")} aria-pressed={showThreads}>
            <ConsoleIcon.history />
          </ZButton>
          {turns.length > 0 && !streaming && (
            <Tooltip title={t("panel.newThread")}>
              <Button size="small" type="text" icon={<PlusOutlined />} onClick={() => { assistant.reset(); setPicked(undefined); setShowThreads(false); }} aria-label={t("panel.newThread")} />
            </Tooltip>
          )}
          <Button size="small" type="text" icon={<CloseOutlined />} onClick={() => setOpen(false)} aria-label={t("panel.close")} />
        </Space>
      }
    >
      <div style={{ flex: 1, overflowY: "auto", padding: 16 }}>
        {showThreads ? (
          <ThreadList
            threads={assistant.threads}
            current={assistant.threadID}
            disabled={streaming}
            onOpen={(id) => { assistant.openThread(id); setShowThreads(false); }}
            onDelete={assistant.deleteThread}
          />
        ) : thread ? (
          <Space direction="vertical" size={20} style={{ width: "100%" }}>
            {turns.map((turn, i) => (
              <TurnView key={turn.id} turn={turn} first={i === 0} last={i === turns.length - 1} onFollowUp={send} />
            ))}
          </Space>
        ) : (
          <PageTasks subjects={pageSubjects} />
        )}
      </div>
      <div style={{ borderTop: "1px solid rgba(0,0,0,0.06)", padding: 12 }} onDragOver={(e) => e.preventDefault()} onDrop={onDrop}>
        {!thread && pageSubjects.length > 0 && (
          <Space size={4} wrap style={{ marginBottom: 8 }} aria-label={t("panel.about")}>
            <Text type="secondary" style={{ fontSize: 12 }}>{t("panel.about")}</Text>
            {pageSubjects.map((p) => (
              <Tag.CheckableTag key={`${p.subject.kind}:${p.subject.label}`} checked={sameSubject(about, p.subject)} onChange={(on) => setPicked(on ? p.subject : null)}>
                {t(`subject.${p.subject.kind}`)} {p.subject.label}
              </Tag.CheckableTag>
            ))}
          </Space>
        )}
        {thread?.subject && (
          <Text type="secondary" style={{ display: "block", fontSize: 12, marginBottom: 8 }}>
            {t("panel.threadAbout", { subject: `${t(`subject.${thread.subject.kind}`)} ${thread.subject.label}` })}
          </Text>
        )}
        {images.length > 0 && (
          <div className="mb-2 flex flex-wrap gap-2" aria-label={t("panel.attached")}>
            {images.map((url, i) => (
              <div key={i} className="relative">
                <img src={url} alt={t("panel.imageAlt", { n: i + 1 })} className="h-14 w-14 rounded-md border border-border object-cover" />
                <ZButton variant="secondary" size="xs" iconOnly className="absolute -right-2 -top-2" aria-label={t("panel.removeImage", { n: i + 1 })}
                  onClick={() => setImages((all) => all.filter((_, j) => j !== i))}>
                  <ConsoleIcon.close />
                </ZButton>
              </div>
            ))}
          </div>
        )}
        {imageNote && <p role="status" className="mb-2 mt-0 text-xs text-fg-muted">{imageNote}</p>}
        <Space.Compact style={{ width: "100%" }}>
          {imageLimit && (
            <>
              <input ref={fileInput} type="file" accept={IMAGE_TYPES.join(",")} multiple hidden aria-label={t("panel.attachImage")}
                onChange={(e) => { void attach([...(e.target.files ?? [])]); e.target.value = ""; }} />
              <ZButton variant="secondary" size="md" iconOnly title={t("panel.attachHint")} onClick={() => fileInput.current?.click()} disabled={streaming || images.length >= imageLimit.per_message} aria-label={t("panel.attachImage")}>
                <ConsoleIcon.imageAdd />
              </ZButton>
            </>
          )}
          <Input.TextArea
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onPaste={onPaste}
            onPressEnter={(e) => { if (!e.shiftKey) { e.preventDefault(); send(draft, images); } }}
            autoSize={{ minRows: 1, maxRows: 4 }}
            placeholder={thread ? t("panel.followUpPlaceholder") : t("panel.askPlaceholder")}
            aria-label={t("panel.input")}
            disabled={streaming}
          />
          <Button type="primary" icon={<SendOutlined />} onClick={() => send(draft, images)} disabled={streaming || (!draft.trim() && !images.length)} aria-label={t("panel.send")} />
        </Space.Compact>
      </div>
    </Drawer>
  );
}

// ThreadList is the account's kept threads, newest first: open one to read
// and continue it, or delete it.
function ThreadList({ threads, current, disabled, onOpen, onDelete }: { threads: Thread[]; current: string | null; disabled: boolean; onOpen: (id: string) => void; onDelete: (id: string) => void }) {
  const { t, i18n } = useTranslation("ai");
  const when = new Intl.DateTimeFormat(i18n.resolvedLanguage === "zh" ? "zh-CN" : "en", { dateStyle: "short", timeStyle: "short" });
  return (
    <section className="flex flex-col gap-2" aria-label={t("panel.history")}>
      <h3 className="m-0 text-sm font-semibold text-fg-default">{t("panel.history")}</h3>
      <p className="m-0 text-xs text-fg-muted">{t("panel.historyHint")}</p>
      {threads.length === 0 ? (
        <p className="m-0 py-6 text-center text-sm text-fg-muted">{t("panel.noThreads")}</p>
      ) : (
        <ul aria-label={t("panel.history")} className="m-0 flex list-none flex-col gap-1 p-0">
          {threads.map((thread) => (
            <li key={thread.id} className={`flex items-center gap-1 rounded-md border px-1 py-1 ${thread.id === current ? "border-border bg-surface-raised" : "border-border-subtle"}`}>
              <button type="button" disabled={disabled} onClick={() => onOpen(thread.id)} aria-current={thread.id === current || undefined}
                className="flex min-w-0 flex-1 cursor-pointer flex-col items-start gap-0.5 rounded-md border-0 bg-transparent px-2 py-1 text-left hover:bg-surface-raised focus-visible:outline-2 focus-visible:outline-focus-ring disabled:cursor-not-allowed disabled:opacity-50">
                <span className="w-full truncate text-sm text-fg-default">{thread.title || t("panel.untitled")}</span>
                <span className="text-xs text-fg-muted">{when.format(thread.updatedAt)}</span>
              </button>
              <ZButton variant="ghost" size="sm" iconOnly className="hover:text-fg-danger" disabled={disabled && thread.id === current} onClick={() => onDelete(thread.id)} aria-label={t("panel.deleteThread", { title: thread.title })}>
                <ConsoleIcon.delete />
              </ZButton>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

// PageTasks is the empty panel: what the current page shows, attention first,
// each with the tasks it supports as buttons.
function PageTasks({ subjects }: { subjects: PageSubject[] }) {
  const { t } = useTranslation("ai");
  const { start } = useAssistant();
  const ordered = [...subjects.filter((s) => s.attention), ...subjects.filter((s) => !s.attention)].filter((s) => s.tasks.length);
  if (ordered.length === 0) {
    return <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={<Space direction="vertical" size={4}><span>{t("panel.emptyTitle")}</span><Text type="secondary" style={{ fontSize: 12 }}>{t("panel.emptyHint")}</Text></Space>} />;
  }
  return (
    <Space direction="vertical" size={12} style={{ width: "100%" }}>
      <Text strong>{t("panel.onThisPage")}</Text>
      {ordered.map((p) => (
        <div key={`${p.subject.kind}:${p.subject.label}`} style={{ border: "1px solid rgba(0,0,0,0.08)", borderRadius: 6, padding: 12 }}>
          <Space direction="vertical" size={8} style={{ width: "100%" }}>
            <Text>{t(`subject.${p.subject.kind}`)} <Text strong>{p.subject.label}</Text></Text>
            {p.attention && <Text type="warning"><WarningOutlined /> {p.attention}</Text>}
            <Space size={8} wrap>
              {p.tasks.map((task) => (
                <Button key={task} size="small" type={p.attention ? "primary" : "default"} onClick={() => start(task, p.subject)}>
                  {t(`task.${task}.action`)}
                </Button>
              ))}
            </Space>
          </Space>
        </div>
      ))}
    </Space>
  );
}

// TurnView is one question and its answer: the platform's facts first (only
// on a task's first turn), then the streamed answer with its steps.
function TurnView({ turn, first, last, onFollowUp }: { turn: Turn; first: boolean; last: boolean; onFollowUp: (q: string) => void }) {
  const { t } = useTranslation("ai");
  const { retry } = useAssistant();
  const answer = turn.answer;
  const live = "live" in answer ? answer.live : { text: "", reasoning: "", steps: [] };
  const streaming = answer.status === "streaming";
  return (
    <section aria-label={turn.question || t(`task.${turn.task}.title`)}>
      <Space direction="vertical" size={8} style={{ width: "100%" }}>
        {turn.images?.length ? (
          <div className="ml-auto flex flex-wrap justify-end gap-2" aria-label={t("panel.attached")}>
            {turn.images.map((url, i) => (
              <img key={i} src={url} alt={t("panel.imageAlt", { n: i + 1 })} className="max-h-32 max-w-40 rounded-md border border-border object-contain" />
            ))}
          </div>
        ) : null}
        {turn.question ? (
          <div style={{ alignSelf: "flex-end", marginLeft: "auto", maxWidth: "85%", background: "#1f5fd6", color: "#fff", padding: "6px 10px", borderRadius: "10px 10px 2px 10px", whiteSpace: "pre-wrap" }}>
            {turn.question}
          </div>
        ) : (
          first && !turn.images?.length && (
            <Text strong>
              {t(`task.${turn.task}.title`)}
              {turn.subject && <Text type="secondary" style={{ fontWeight: 400 }}> · {turn.subject.label}</Text>}
            </Text>
          )
        )}
        {turn.facts.status === "loading" && <Spin size="small" />}
        {turn.facts.status === "error" && <Alert type="error" showIcon message={turn.facts.message} />}
        {turn.facts.status === "done" && ("findings" in turn.facts.value ? <FindingCards findings={turn.facts.value.findings} /> : <FitTable fit={turn.facts.value.fit} />)}
        {answer.status === "unconfigured" && <Alert type="info" showIcon message={t("configureFirst")} />}
        {streaming && !live.text && (
          <Space size={8}>
            <Spin size="small" />
            <Text type="secondary">{live.reasoning ? t("thinking") : live.steps.length ? t("investigating") : t("loading")}</Text>
          </Space>
        )}
        <StepList steps={live.steps} />
        {streaming && !live.text && live.reasoning && <ReasoningTail text={live.reasoning} label={t("reasoning")} />}
        {live.reasoning && !(streaming && !live.text) && (
          <details>
            <summary style={{ cursor: "pointer", fontSize: 12, color: "rgba(0,0,0,0.55)" }}>{t("reasoning")}</summary>
            <div style={{ whiteSpace: "pre-wrap", fontSize: 12, color: "rgba(0,0,0,0.55)", marginTop: 4 }}>{live.reasoning}</div>
          </details>
        )}
        {live.text && (
          <div aria-live="polite" style={{ background: "#f5f7fa", border: "1px solid #f0f2f5", borderRadius: 2, padding: 12, fontSize: 14 }}>
            <MarkdownContent content={live.text} />
          </div>
        )}
        {answer.status === "done" && !live.text && <Text type="secondary">{t("empty")}</Text>}
        {answer.status === "done" && <AnswerSummary done={answer.done} tools={live.steps.length} />}
        {answer.status === "stopped" && <Alert type="warning" showIcon message={t("stopped")} action={<Button size="small" onClick={() => retry(turn.id)}>{t("retry")}</Button>} />}
        {answer.status === "error" && <Alert type="error" showIcon message={t("requestFailed")} description={answer.message} action={<Button size="small" onClick={() => retry(turn.id)}>{t("retry")}</Button>} />}
        {last && answer.status === "done" && followUps[turn.task].length > 0 && (
          <Space size={[6, 6]} wrap aria-label={t("panel.followUps")}>
            {followUps[turn.task].map((key) => (
              <Button key={key} size="small" onClick={() => onFollowUp(t(`followUp.${turn.task}.${key}`))}>
                {t(`followUp.${turn.task}.${key}`)}
              </Button>
            ))}
          </Space>
        )}
      </Space>
    </section>
  );
}
