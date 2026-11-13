import { useMemo, type TableHTMLAttributes } from "react";
import { Table, Tag, Typography } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { AcquisitionNodeFact, AcquisitionResult } from "../../api";
import { APIError, apiErrorText } from "../../apiError";
import { diagnosticMessageText } from "./deployDiagnosticText";

// refSource names where a pull candidate lives: the registry host of an image
// reference, or the host of an archive URL. A reference without a registry is
// Docker Hub's.
function refSource(ref: string): string {
  if (/^https?:\/\//i.test(ref)) {
    try {
      return new URL(ref).host;
    } catch {
      return ref;
    }
  }
  const first = ref.split("/")[0];
  const hasHost =
    ref.includes("/") &&
    (first.includes(".") || first.includes(":") || first === "localhost");
  return hasHost ? first : "docker.io";
}

// pullPlanText says where a node will pull the image from: rule endpoints in
// their order first, then the image's own registries, which the node orders
// by its probe when there are several.
function pullPlanText(node: AcquisitionNodeFact, t: TFunction<"deploy">): string {
  const refs = node.image_pull_refs || [];
  const fixedCount = Math.min(node.image_pull_fixed ?? 0, refs.length);
  const hosts = (list: string[]) => [...new Set(list.map(refSource))];
  const fixed = hosts(refs.slice(0, fixedCount));
  const rest = hosts(refs.slice(fixedCount)).filter((host) => !fixed.includes(host));
  const probed = Boolean(node.image_pull_ranked) && rest.length > 1;
  const sources = rest.join(" / ");
  if (fixed.length === 0) {
    if (rest.length === 0) return t("workbench.nodeAcquisition.pull");
    return probed
      ? t("workbench.nodeAcquisition.pullRanked", { sources })
      : t("workbench.nodeAcquisition.pullFrom", { source: sources });
  }
  const first = fixed.join(" → ");
  if (rest.length === 0) return t("workbench.nodeAcquisition.pullFixed", { sources: first });
  return probed
    ? t("workbench.nodeAcquisition.pullFixedThenRanked", { fixed: first, sources })
    : t("workbench.nodeAcquisition.pullFixedThen", { fixed: first, source: sources });
}

function providerName(provider: string, t: TFunction<"deploy">): string {
  if (provider === "modelscope") return t("action.modelScope");
  if (provider === "huggingface") return t("action.huggingFace");
  return provider;
}

function Muted({ children }: { children: string }) {
  return (
    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
      {children}
    </Typography.Text>
  );
}

function ImageCell({
  node,
  t,
  tCommon,
}: {
  node: AcquisitionNodeFact;
  t: TFunction<"deploy">;
  tCommon: TFunction<"common">;
}) {
  // A node whose cache cannot be read cannot be forecast either.
  if (
    node.image_cached === undefined &&
    (node.image_cache_reason === "node_offline" ||
      node.image_cache_reason === "node_unselected")
  ) {
    return (
      <Tag>
        {t(
          node.image_cache_reason === "node_offline"
            ? "workbench.nodeAcquisition.unknownOffline"
            : "workbench.nodeAcquisition.unknownUnselected",
        )}
      </Tag>
    );
  }
  switch (node.image_plan) {
    case "present":
      return <Tag color="green">{t("workbench.nodeAcquisition.present")}</Tag>;
    case "copy":
      return (
        <span>
          {t("workbench.nodeAcquisition.copyFrom", {
            from: node.image_copy_from || "",
          })}{" "}
          <Muted>{t("workbench.nodeAcquisition.sameGroup")}</Muted>
        </span>
      );
    case "pull": {
      if (node.image_pull_error)
        return (
          <Typography.Text type="danger">
            {t("workbench.nodeAcquisition.pullError", {
              detail: apiErrorText(
                tCommon,
                new APIError(
                  node.image_pull_error.code,
                  node.image_pull_error.params,
                  node.image_pull_error.message,
                  200,
                ),
              ),
            })}
          </Typography.Text>
        );
      const refs = node.image_pull_refs || [];
      return <span title={refs.join("\n")}>{pullPlanText(node, t)}</span>;
    }
    default:
      return <Tag>{t("workbench.nodeAcquisition.unknown")}</Tag>;
  }
}

function WeightsCell({
  node,
  t,
  tCommon,
  declaredProviders,
}: {
  node: AcquisitionNodeFact;
  t: TFunction<"deploy">;
  tCommon: TFunction<"common">;
  declaredProviders: string[];
}) {
  switch (node.weights_plan) {
    case "unavailable":
      return <Typography.Text type="danger">{t("workbench.nodeAcquisition.unavailable", { detail: node.weights_fetch_error ? diagnosticMessageText(t, tCommon, node.weights_fetch_error) : t("workbench.nodeAcquisition.unknown") })}</Typography.Text>;
    case "present":
      return <Tag color="green">{t("workbench.nodeAcquisition.present")}</Tag>;
    case "copy":
      return (
        <span>
          {t("workbench.nodeAcquisition.copyFrom", {
            from: node.weights_copy_from || "",
          })}{" "}
          <Muted>{t("workbench.nodeAcquisition.sameGroup")}</Muted>
        </span>
      );
    case "storage":
      return (
        <span>
          {t("workbench.nodeAcquisition.readFromStorage", {
            name: node.weights_storage || "",
          })}
        </span>
      );
    case "download": {
      const error = node.weights_fetch_error;
      if (error) {
        // Only a prohibition has another way out: a source the template also
        // declares. It is a hint; nothing here changes the choice.
        const chosen = (node.weights_provider || "").toLowerCase();
        const others = [
          ...new Set(declaredProviders.map((provider) => provider.toLowerCase())),
        ].filter((provider) => provider !== chosen);
        return (
          <span>
            <Typography.Text type="danger">
              {t("workbench.nodeAcquisition.downloadError", {
                detail: apiErrorText(
                  tCommon,
                  new APIError(error.code, error.params, error.message, 200),
                ),
              })}
            </Typography.Text>
            {error.code === "WEIGHTS_FETCH_FORBIDDEN" && others.length > 0 && (
              <>
                {" "}
                <Muted>
                  {t("workbench.nodeAcquisition.downloadOtherSource", {
                    provider: others
                      .map((provider) => providerName(provider, t))
                      .join(" / "),
                  })}
                </Muted>
              </>
            )}
          </span>
        );
      }
      const hosts = node.weights_fetch_hosts || [];
      if (hosts.length === 0)
        return (
          <span>
            {t("workbench.nodeAcquisition.downloadFrom", {
              provider: node.weights_provider || "",
            })}
          </span>
        );
      return (
        <span title={hosts.join("\n")}>
          {t(
            hosts.length > 1
              ? "workbench.nodeAcquisition.downloadHostsOrdered"
              : "workbench.nodeAcquisition.downloadHost",
            { hosts: hosts.join(" → ") },
          )}
        </span>
      );
    }
    default:
      return <Tag>{t("workbench.nodeAcquisition.unknown")}</Tag>;
  }
}

/**
 * How the image and the weights are expected to reach the nodes of the draft.
 * With the nodes named (a manual pick or a multi-node draft) the server
 * forecasts each one; with automatic placement there is no node yet, so the
 * row only says the platform fetches after scheduling, following the pull
 * settings. Either way it is information: nothing here is a choice, the
 * replica decides again when it starts.
 */
export default function DeployNodeAcquisition({
  acquisition,
  automatic,
  loading = false,
  declaredProviders = [],
}: {
  acquisition?: AcquisitionResult;
  /** The form places the replica automatically (no node of its own yet). */
  automatic: boolean;
  /** A re-evaluation is in flight: the forecast still answers the previous draft. */
  loading?: boolean;
  /** The weight providers the chosen template spec declares (for the other-source hint). */
  declaredProviders?: string[];
}) {
  const { t } = useTranslation("deploy");
  const { t: tCommon } = useTranslation("common");
  const nodes = acquisition?.nodes ?? [];
  const caption = t("workbench.nodeAcquisition.forecastNote");
  // The table's caption names it for assistive technology and reads as the
  // note above the rows.
  const components = useMemo(
    () => ({
      table: (props: TableHTMLAttributes<HTMLTableElement>) => (
        <table {...props}>
          <caption
            style={{ captionSide: "top", textAlign: "start", padding: "0 0 8px" }}
          >
            <Typography.Text type="secondary">{caption}</Typography.Text>
          </caption>
          {props.children}
        </table>
      ),
    }),
    [caption],
  );
  if (!acquisition) return null;
  if (nodes.length === 0 && !automatic) return null;
  return (
    <div className="deploy-workbench-row">
      <div className="deploy-workbench-label">
        {t("workbench.nodeAcquisition.title")}
      </div>
      <div className="deploy-workbench-body">
        {nodes.length === 0 ? (
          <Typography.Text type="secondary">
            {t("workbench.nodeAcquisition.automatic")}{" "}
            <Link to="/settings">{t("workbench.nodeAcquisition.viewSettings")}</Link>
          </Typography.Text>
        ) : (
          <Table<AcquisitionNodeFact>
            size="small"
            rowKey="node_uuid"
            pagination={false}
            loading={loading}
            dataSource={nodes}
            components={components}
            columns={[
              {
                title: t("workbench.nodeAcquisition.node"),
                dataIndex: "node_name",
              },
              {
                title: t("workbench.nodeAcquisition.image"),
                key: "image",
                render: (_, node) => <ImageCell node={node} t={t} tCommon={tCommon} />,
              },
              {
                title: t("workbench.nodeAcquisition.weights"),
                key: "weights",
                render: (_, node) => (
                  <WeightsCell
                    node={node}
                    t={t}
                    tCommon={tCommon}
                    declaredProviders={declaredProviders}
                  />
                ),
              },
            ]}
          />
        )}
      </div>
    </div>
  );
}
