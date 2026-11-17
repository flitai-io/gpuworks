import { Button, Tooltip } from "antd";
import { RobotOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { useOptionalAssistant, type AssistantSubject, type AssistantTask } from "../assistant/AssistantContext";
import { Button as ZeronButton } from "./ui/button";
import { Tooltip as ZeronTooltip } from "./ui/tooltip";
import { ConsoleIcon } from "../ui-kit/consoleIcons";

const zeronSize = { small: "xs", middle: "sm", large: "md" } as const;

// AIAssistant is a one-click entry to the assistant: it starts task about
// subject in the resident side panel. The user writes no prompt; the server
// gathers the subject's context for the task. Outside the app shell (no
// assistant provided) it renders nothing rather than a dead button.
export default function AIAssistant({
  task,
  subject,
  buttonText,
  buttonType = "default",
  buttonSize = "middle",
}: {
  task: AssistantTask;
  subject: AssistantSubject;
  buttonText?: string;
  buttonType?: "default" | "primary" | "link" | "inline" | "tertiary";
  buttonSize?: "small" | "middle" | "large";
}) {
  const { t } = useTranslation("ai");
  const assistant = useOptionalAssistant();
  const label = buttonText || t(`task.${task}.action`);
  if (!assistant) return null;
  const start = () => assistant.start(task, subject);
  if (buttonType === "inline" || buttonType === "tertiary") return (
    <ZeronTooltip content={t("tooltip")}>
      <ZeronButton variant={buttonType === "inline" ? "link" : "tertiary"} size={zeronSize[buttonSize]} leadingIcon={ConsoleIcon.assistant} onClick={start}>{label}</ZeronButton>
    </ZeronTooltip>
  );
  return (
    <Tooltip title={t("tooltip")}>
      <Button type={buttonType} size={buttonSize} icon={<RobotOutlined />} onClick={start} aria-label={label}>
        {buttonText ?? label}
      </Button>
    </Tooltip>
  );
}
