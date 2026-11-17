import { useEffect, useRef, useState } from "react";
import { Alert, Button, Space, Tag, Typography } from "antd";
import { ReloadOutlined } from "@ant-design/icons";
import { terminalOptions } from "../theme/terminalTheme";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import {
  openTerminalSocket,
  terminalPreflight,
  type TerminalInfo,
  type TerminalSocket,
  type TerminalTarget,
} from "../terminal";
import { useTranslation } from "react-i18next";
import { apiErrorText } from "../apiError";
import { terminalPreflightReason } from "../terminalMessages";

// This module is loaded lazily so xterm (~250 KB) stays out of the main bundle;
// only operators who actually open a terminal download it.

interface Props {
  nodeUUID: string;
  hostname?: string;
  target?: TerminalTarget;
  ref_?: string; // container id for the container target
}

type Status =
  | { kind: "connecting" }
  | { kind: "open" }
  | { kind: "exited"; code: number; reason: string }
  | { kind: "error"; message: string };

export default function NodeTerminal({ nodeUUID, hostname, target = "host", ref_ }: Props) {
  const { t } = useTranslation("nodes");
  const { t: tCommon } = useTranslation("common");
  const holder = useRef<HTMLDivElement | null>(null);
  const termRef = useRef<Terminal | null>(null);
  const sockRef = useRef<TerminalSocket | null>(null);
  const [status, setStatus] = useState<Status>({ kind: "connecting" });
  const [info, setInfo] = useState<TerminalInfo | null>(null);
  const [containerRuntime, setContainerRuntime] = useState("");
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const node = holder.current;
    if (!node) return;

    let disposed = false;
    const term = new Terminal({
      cursorBlink: true,
      ...terminalOptions,
      scrollback: 5000,
      // Matches the TERM=xterm-256color the agent exports.
      allowProposedApi: true,

    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(node);
    fit.fit();
    termRef.current = term;

    let sock: TerminalSocket | null = null;

    // The preflight runs first because a browser cannot read the HTTP status of a
    // failed WebSocket handshake: without it "disabled by an admin", "node
    // offline", "agent too old" and "session limit reached" would all look like
    // an unexplained connection failure.
    (async () => {
      try {
        const pre = await terminalPreflight(nodeUUID);
        if (disposed) return;
        setContainerRuntime(target === "container" ? (pre.container_runtime ?? "") : "");
        if (!pre.can_open) {
          setStatus({ kind: "error", message: terminalPreflightReason(pre.reason, t) || t("terminalView.cannotOpen") });
          return;
        }
        if (target === "container" && !pre.container_supported) {
          setStatus({ kind: "error", message: terminalPreflightReason(pre.container_reason, t) || t("terminalView.containerUnsupported") });
          return;
        }

        sock = openTerminalSocket({
          nodeUUID,
          target,
          ref: ref_,
          rows: term.rows,
          cols: term.cols,
          onData: (bytes) => {
            // Passing bytes (not a decoded string) lets xterm's own UTF-8 decoder
            // reassemble multi-byte characters split across frames, which is what
            // keeps Chinese output and nvidia-smi's box drawing intact.
            term.write(bytes);
          },
          onInfo: (i) => setInfo(i),
          onExit: (code, reason) => {
            setStatus({ kind: "exited", code, reason });
            term.write(`\r\n\x1b[33m[${t("terminalView.sessionEnded")}] code=${code}${reason ? ` (${reason})` : ""}\x1b[0m\r\n`);
          },
          onError: (message) => setStatus({ kind: "error", message }),
          onClose: () => {
            setStatus((s) => (s.kind === "open" || s.kind === "connecting" ? { kind: "exited", code: -1, reason: t("terminalView.connectionClosed") } : s));
          },
        });
        sockRef.current = sock;
        setStatus({ kind: "open" });
        term.onData((d) => sock?.send(d));
        term.focus();
      } catch (err) {
        if (!disposed) {
          setStatus({ kind: "error", message: apiErrorText(tCommon, err) });
        }
      }
    })();

    // Refit on container resize and tell the agent, so full-screen programs
    // receive SIGWINCH and redraw at the new size.
    let raf = 0;
    const observer = new ResizeObserver(() => {
      cancelAnimationFrame(raf);
      raf = requestAnimationFrame(() => {
        try {
          fit.fit();
        } catch {
          return; // element not laid out yet
        }
        sockRef.current?.resize(term.rows, term.cols);
      });
    });
    observer.observe(node);

    return () => {
      disposed = true;
      observer.disconnect();
      cancelAnimationFrame(raf);
      // Closing the socket is what makes the server tear the pty down, which
      // reaps the whole terminal session on the node.
      sock?.close();
      sockRef.current = null;
      term.dispose();
      termRef.current = null;
    };
  }, [nodeUUID, target, ref_, attempt, t, tCommon]);

  const label = hostname || nodeUUID;
  const closed = status.kind === "exited" || status.kind === "error";

  return (
    <div className="node-terminal" style={{ background: terminalOptions.theme.background }}>
      <div className="node-terminal-bar">
        <Space size={8} wrap>
          <Typography.Text strong style={{ color: "#e6edf5" }}>
            {t(target === "container" ? "terminalView.container" : "terminalView.host")} · {label}
          </Typography.Text>
          {status.kind === "connecting" && <Tag color="processing">{t("terminalView.status.connecting")}</Tag>}
          {status.kind === "open" && <Tag color="green">{t("terminalView.status.open")}</Tag>}
          {status.kind === "exited" && <Tag color="default">{t("terminalView.status.exited")}</Tag>}
          {status.kind === "error" && <Tag color="red">{t("terminalView.status.error")}</Tag>}
          {info && (
            // The agent runs as root under systemd but as the installing user via
            // the nohup fallback, so the real account is shown rather than assumed.
            <Tag color={info.uid === 0 ? "volcano" : "blue"}>
              {info.user}
              {info.uid >= 0 ? ` (uid ${info.uid})` : ""}
            </Tag>
          )}
          {info?.shell && <Typography.Text type="secondary" style={{ fontSize: 12 }}>{info.shell}</Typography.Text>}
        </Space>
        {closed && (
          <Button size="small" icon={<ReloadOutlined />} onClick={() => { setInfo(null); setStatus({ kind: "connecting" }); setAttempt((n) => n + 1); }}>
            {t("terminalView.reconnect")}
          </Button>
        )}
      </div>

      {status.kind === "error" && (
        <Alert type="error" showIcon banner message={status.message} style={{ margin: 0 }} />
      )}
      {target === "container" && containerRuntime === "udocker" && status.kind !== "error" && (
        <Alert type="info" showIcon banner message={t("terminalView.runveilShell")} style={{ margin: 0 }} />
      )}

      <div className="node-terminal-surface" ref={holder} />
    </div>
  );
}
