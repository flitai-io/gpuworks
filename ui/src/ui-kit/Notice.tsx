import type { ComponentProps, ReactNode } from "react";
import { InfoCircleOutlined, WarningOutlined } from "@ant-design/icons";
import { Alert, AlertTitle, AlertDescription, AlertIcon, AlertAction, type AlertStatus } from "../components/ui/alert";
/** Localized in-context feedback composed from the public Zeron alert parts. */
export function Notice({ tone = "info", title, children, action, ...alert }: Pick<ComponentProps<typeof Alert>, "className" | "role"> & { tone?: AlertStatus; title: ReactNode; children?: ReactNode; action?: ReactNode }) {
  return <Alert status={tone} {...alert}>
    <AlertIcon>{tone === "danger" || tone === "warning" ? <WarningOutlined /> : <InfoCircleOutlined />}</AlertIcon>
    <AlertTitle>{title}</AlertTitle>
    {children && <AlertDescription>{children}</AlertDescription>}
    {action && <AlertAction>{action}</AlertAction>}
  </Alert>;
}
