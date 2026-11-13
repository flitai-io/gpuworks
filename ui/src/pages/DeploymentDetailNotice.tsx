import type { ReactNode } from "react";
import { Notice } from "../ui-kit/Notice";

export default function DeploymentDetailNotice({ type, message, description, action }: {
  type: "error" | "warning" | "info"; message: ReactNode; description?: ReactNode; action?: ReactNode;
}) {
  return (
    <Notice className="detail-notice" role="alert" tone={type === "error" ? "danger" : type} title={message}
      action={action && <div className="detail-notice-actions">{action}</div>}>
      {description}
    </Notice>
  );
}
