import type { ReactNode } from "react";
import { Tag, Typography } from "antd";
import type { TFunction } from "i18next";
import type { DeploymentInfo, KeyInfo } from "./usageModel";

/** keyTitle names a token the way every analytics view does: a deleted one by its id, a keyless Playground, console batch or evaluation request by its user. */
export function keyTitle(t: TFunction, key: KeyInfo): { title: string; sub: string } {
  if (key.deleted) return { title: t("names.deletedKey", { id: key.id }), sub: "" };
  if (key.playground) return { title: key.userName ? t("names.playground", { user: key.userName }) : t("names.playgroundAnonymous"), sub: "" };
  if (key.batch) return { title: key.userName ? t("names.batch", { user: key.userName }) : t("names.batchAnonymous"), sub: "" };
  if (key.evaluation) return { title: key.userName ? t("names.evaluation", { user: key.userName }) : t("names.evaluationAnonymous"), sub: "" };
  if (!key.id) return { title: t("names.noKey"), sub: "" };
  return { title: key.name || key.prefix, sub: key.name ? key.prefix : "" };
}

export function deploymentTitle(t: TFunction, d: DeploymentInfo): string {
  return d.name || t("names.deploymentId", { id: d.id });
}

/** NameCell is a primary line (with tags) over a muted secondary line. */
export function NameCell({ title, sub, tags, monoSub }: { title: ReactNode; sub?: ReactNode; tags?: ReactNode; monoSub?: boolean }) {
  return (
    <div>
      <div style={{ fontWeight: 500 }}>{title}{tags}</div>
      {sub ? <Typography.Text type="secondary" style={{ fontSize: 12, fontFamily: monoSub ? "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace" : undefined }}>{sub}</Typography.Text> : null}
    </div>
  );
}

export function KeyCell({ t, info }: { t: TFunction; info: KeyInfo }) {
  const { title, sub } = keyTitle(t, info);
  return <NameCell title={title} sub={sub} monoSub tags={info.revoked ? <Tag style={{ marginLeft: 8 }}>{t("names.revoked")}</Tag> : null} />;
}

export function DeploymentCell({ t, deployment }: { t: TFunction; deployment: DeploymentInfo }) {
  return <NameCell title={deploymentTitle(t, deployment)} sub={deployment.served} tags={deployment.deleted ? <Tag color="warning" style={{ marginLeft: 8 }}>{t("names.deletedDeployment")}</Tag> : null} />;
}
