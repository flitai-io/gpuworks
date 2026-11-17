import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const zh = (await createI18n(["nodes"], "zh")).i18n;
const { cleanup, render, screen, within } = await import("@testing-library/react");
const { default: NodeDoctorReport } = await import("../src/components/NodeDoctorReport.tsx");
type Report = import("../src/api.ts").NodeDoctorReport;
afterEach(() => cleanup());

const report: Report = {
  report: "partial report",
    checked_at: "2026-11-09T10:00:00Z", summary: "fail",
    checks: [
      { stage: "connection", status: "fail", code: "TIMEOUT", vantage: "control_plane", target: "node.example:18800", source: "tcp_probe", observed_at: "2026-11-09T09:59:58Z" },
      { stage: "authentication", status: "skip", code: "AUTHENTICATION_NOT_CHECKED", vantage: "control_plane" },
      { stage: "accelerators", status: "skip", code: "REQUIRES_AGENT", vantage: "node" },
    ],

};

test("offline doctor renders unavailable checks separately from connection failure", () => {
  render(<I18nextProvider i18n={zh}><NodeDoctorReport report={report} /></I18nextProvider>);
  const connection = screen.getByRole("region", { name: "连接建立" });
  assert.ok(within(connection).getByText("失败"));
  assert.ok(within(connection).getByText("node.example:18800"));
  assert.ok(within(connection).getByText(/尚不能确定阻断位置/));
  const auth = screen.getByRole("region", { name: "身份认证" });
  assert.ok(within(auth).getByText("未检查"));
  assert.equal(within(auth).queryByText("失败"), null);
  assert.equal(within(auth).queryByText("通过"), null);
  assert.ok(within(auth).getByText(/没有执行 Agent 身份认证/));
  assert.ok(screen.getByText("需要 Agent 已连接后才能执行检查。"));
  assert.equal(screen.queryByText("partial report"), null);
});

test("access diagnosis renders English evidence and keeps legacy reports working", async () => {
  const en = (await createI18n(["nodes"], "en")).i18n;
  const view = render(<I18nextProvider i18n={en}><NodeDoctorReport report={report} /></I18nextProvider>);
  assert.ok(screen.getByRole("region", { name: "Authentication" }));
  assert.ok(screen.getAllByText("Not checked").length === 2);
  assert.ok(screen.getByText(/blocking location is unknown/));
  view.rerender(<I18nextProvider i18n={en}><NodeDoctorReport report={{ report: "legacy result" }} /></I18nextProvider>);
  assert.ok(screen.getByText("legacy result"));
  assert.equal(screen.queryByRole("region", { name: "Authentication" }), null);
});

test("rerun replaces prior failures with current connection evidence", () => {
  const view = render(<I18nextProvider i18n={zh}><NodeDoctorReport report={report} /></I18nextProvider>);
  const next: Report = { ...report, report: "", summary: "pass", checks: [
    { stage: "connection", status: "pass", code: "TCP_CONNECTED", vantage: "control_plane", target: "node.example:18800", source: "tcp_probe" },
  ] };
  view.rerender(<I18nextProvider i18n={zh}><NodeDoctorReport report={next} /></I18nextProvider>);
  const connection = screen.getByRole("region", { name: "连接建立" });
  assert.equal(within(connection).queryByText("失败"), null);
  assert.equal(within(connection).queryByText(/尚不能确定阻断位置/), null);
  assert.ok(within(connection).getByText("通过"));
  assert.ok(within(connection).getByText("node.example:18800"));
  assert.ok(within(connection).getByText("目标 TCP 端口已连通"));
});


test("node HTTP success is shown separately from unavailable authentication", () => {
  const current: Report = { ...report, report: "", summary: "pass", checks: [
    { stage: "connection", status: "pass", code: "HTTP_CONNECTED", vantage: "node", target: "http://server:8800/healthz", source: "http_probe" },
    { stage: "authentication", status: "skip", code: "AUTHENTICATION_NOT_CHECKED", vantage: "node" },
  ] };
  render(<I18nextProvider i18n={zh}><NodeDoctorReport report={current} /></I18nextProvider>);
  const connection = screen.getByRole("region", { name: "连接建立" });
  assert.ok(within(connection).getByText("通过"));
  assert.ok(within(connection).getByText("计算节点可访问管理节点的健康检查接口"));
  assert.ok(within(connection).getByText("本次节点侧 HTTP 探测"));
  assert.ok(within(screen.getByRole("region", { name: "身份认证" })).getByText("未检查"));
});

test("one check list renders Agent findings with evidence and preserves hardware details", () => {
  const finding: NonNullable<Report["findings"]>[number] = {
    code: "RUNTIME_PROVISION_FAILED", status: "fail", probe: "runtime setup", evidence: [{ name: "error", value: "permission denied" }], recheck: "check runtime permissions",
  };
  const current: Report = {
    report: "original report", checked_at: report.checked_at, summary: "fail",
    findings: [finding],
    checks: [
      { stage: "host", status: "pass", code: "INFORMATION_COLLECTED", vantage: "node", source: "agent_rpc" },
      { stage: "finding", status: "fail", code: "RUNTIME_PROVISION_FAILED", vantage: "node", source: "agent_rpc", finding },
    ],
    host: { hostname: "test-node", os: "linux", arch: "amd64", kernel: "test-kernel", cpu_model: "test-cpu", cpu_cores: 8, mem_total_bytes: 1024, mem_avail_bytes: 512, disks: [] },
  };
  const view = render(<I18nextProvider i18n={zh}><NodeDoctorReport report={current} /></I18nextProvider>);
  assert.ok(within(screen.getByRole("region", { name: "主机与磁盘检查" })).getByText("通过"));
  const findingRegion = screen.getByRole("region", { name: "运行时初始化失败" });
  assert.ok(within(findingRegion).getByText("失败"));
  assert.equal(screen.getAllByText("permission denied").length, 1);
  assert.equal(screen.getAllByText("check runtime permissions").length, 1);
  assert.ok(within(screen.getByRole("region", { name: "概况" })).getByText(/test-node/));
  assert.equal(screen.queryByRole("region", { name: "需要处理" }), null);
  view.rerender(<I18nextProvider i18n={zh}><NodeDoctorReport report={report} /></I18nextProvider>);
  assert.equal(screen.queryByText("permission denied"), null);
  assert.equal(screen.queryByRole("region", { name: "概况" }), null);
  assert.ok(screen.getByRole("region", { name: "连接建立" }));
});

// Every check status reads as itself in the diagnosis: a stage that did not
// run, one that does not apply, a check skipped on some cards with the reason,
// and a check skipped for a node-wide reason are each worded as not checked
// or not applicable, never as passed.
test("skipped and not-applicable checks are never shown as passed", async () => {
  const { i18n: en, locale } = await createI18n(["nodes"], "en");
  const nodes = locale.nodes;
  const cardSkip: NonNullable<Report["findings"]>[number] = {
    id: "card_runtime", status: "skip", code: "CARD_CHECK_SKIPPED_BUSY",
    targets: [{ index: 2, pcie_bdf: "0000:3f:00.0", evidence: [{ name: "reason", value: "an instance is serving on it" }] }, { index: 3 }],
  };
  const mountSkip: NonNullable<Report["findings"]>[number] = {
    id: "mounts", status: "skip", code: "MOUNT_TABLE_UNREADABLE", probe: "/proc/self/mountinfo", evidence: [{ name: "error", value: "permission denied" }],
  };
  const current: Report = {
    report: "text", checked_at: report.checked_at, summary: "skip",
    findings: [cardSkip, mountSkip],
    checks: [
      { stage: "host", status: "skip", code: "NOT_REPORTED", vantage: "node", source: "agent_rpc" },
      { stage: "runtimes", status: "na", code: "INFORMATION_COLLECTED", vantage: "node", source: "agent_rpc" },
      { stage: "accelerators", status: "pass", code: "INFORMATION_COLLECTED", vantage: "node", source: "agent_rpc" },
      { stage: "finding", status: "skip", code: cardSkip.code, vantage: "node", source: "agent_rpc", finding: cardSkip },
      { stage: "finding", status: "skip", code: mountSkip.code, vantage: "node", source: "agent_rpc", finding: mountSkip },
    ],
  };
  render(<I18nextProvider i18n={en}><NodeDoctorReport report={current} /></I18nextProvider>);
  const status = nodes.doctorChecks.status;
  assert.ok(screen.getByText(nodes.doctorChecks.summary.skip));
  const host = screen.getByRole("region", { name: nodes.doctorChecks.stage.host });
  assert.ok(within(host).getByText(status.skip));
  assert.equal(within(host).queryByText(status.pass), null);
  assert.ok(within(host).getByText(nodes.doctorChecks.code.NOT_REPORTED));
  const runtimes = screen.getByRole("region", { name: nodes.doctorChecks.stage.runtimes });
  assert.ok(within(runtimes).getByText(status.na));
  assert.equal(within(runtimes).queryByText(status.pass), null);
  assert.ok(within(screen.getByRole("region", { name: nodes.doctorChecks.stage.accelerators })).getByText(status.pass));

  const busy = nodes.doctor.notChecked.replace("{{reason}}", nodes.cardHealth.skip.CARD_CHECK_SKIPPED_BUSY);
  const cards = screen.getByRole("region", { name: busy });
  assert.ok(within(cards).getByText(status.skip));
  assert.ok(within(cards).getByText(nodes.doctor.cards.replace("{{count}}", "2").replace("{{indexes}}", "2, 3")));
  // The agent's English wording of the reason is for its CLI; the page words the code.
  assert.equal(within(cards).queryByText("an instance is serving on it"), null);
  assert.equal(within(cards).queryByText(status.pass), null);

  const mounts = screen.getByRole("region", { name: nodes.doctor.finding.MOUNT_TABLE_UNREADABLE.label });
  assert.ok(within(mounts).getByText(status.skip));
  assert.ok(within(mounts).getByText(nodes.doctor.finding.MOUNT_TABLE_UNREADABLE.summary));
  assert.ok(within(mounts).getByText("permission denied"));
  // A skip has no impact statement: it found nothing to have an impact.
  assert.equal(within(mounts).queryByText(nodes.doctor.impact), null);
});

test("a check status this client does not know is shown by its value, not as passed", async () => {
  const { i18n: en, locale } = await createI18n(["nodes"], "en");
  const future = { ...report, summary: "pass", checks: [{ stage: "connection", status: "degraded", code: "FUTURE_CODE", vantage: "node" }] } as unknown as Report;
  render(<I18nextProvider i18n={en}><NodeDoctorReport report={future} /></I18nextProvider>);
  const connection = screen.getByRole("region", { name: locale.nodes.doctorChecks.stage.connection });
  assert.ok(within(connection).getByText("degraded"));
  assert.ok(within(connection).getByText("FUTURE_CODE"));
  assert.equal(within(connection).queryByText(locale.nodes.doctorChecks.status.pass), null);
});

test("an SSH stage shows the SSH adapter's code in the reader's language", async () => {
  const withCommon = (await createI18n(["nodes", "common"], "zh")).i18n;
  const ssh: Report = {
    report: "partial report", checked_at: "2026-11-09T10:00:00Z", summary: "fail",
    checks: [{ stage: "ssh", status: "fail", code: "SSH_AUTH_FAILED", vantage: "control_plane" }],
  };
  render(<I18nextProvider i18n={withCommon}><NodeDoctorReport report={ssh} /></I18nextProvider>);
  assert.ok(screen.getByText("节点拒绝了 SSH 凭据。"));
  assert.equal(screen.queryByText("SSH_AUTH_FAILED"), null);
});
