import { Suspense, lazy, useEffect, useState } from "react";
import { useParams, useSearchParams } from "react-router-dom";
import { Spin, Typography } from "antd";
import { api, type Node } from "../api";
import type { TerminalTarget } from "../terminal";
import { useTranslation } from "react-i18next";
import { useBranding } from "../branding";

// xterm is ~250 KB, so it loads only when a terminal is actually opened rather
// than riding along in the main bundle.
const NodeTerminal = lazy(() => import("../components/NodeTerminal"));

// TerminalPage renders chrome-free at full viewport height: it is opened in its
// own tab so a session survives navigation in the main console, and so an
// operator can read the terminal beside the GPU and log pages.
export default function TerminalPage() {
  const { t } = useTranslation("nodes");
  const { productName } = useBranding();
  const { uuid = "" } = useParams();
  const [params] = useSearchParams();
  const target = (params.get("target") as TerminalTarget) || "host";
  const ref_ = params.get("ref") || undefined;
  const [node, setNode] = useState<Node | null>(null);

  useEffect(() => {
    let cancelled = false;
    // Only for the display name; the terminal itself needs nothing from here.
    api
      .node(uuid)
      .then((n) => {
        if (!cancelled) setNode(n);
      })
      .catch(() => {
        /* the terminal reports its own errors */
      });
    return () => {
      cancelled = true;
    };
  }, [uuid]);

  useEffect(() => {
    const label = node?.hostname || uuid;
    document.title = t("terminalPage.title", { label, product: productName });
  }, [node?.hostname, productName, t, uuid]);

  return (
    <div className="terminal-page">
      <Suspense
        fallback={
          <div className="terminal-page-loading">
            <Spin />
            <Typography.Text type="secondary" style={{ marginLeft: 12 }}>
              {t("terminalPage.loading")}
            </Typography.Text>
          </div>
        }
      >
        <NodeTerminal nodeUUID={uuid} hostname={node?.hostname} target={target} ref_={ref_} />
      </Suspense>
    </div>
  );
}
