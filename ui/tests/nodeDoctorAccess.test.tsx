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
    checked_at: "2026-11-09T10:00:00Z", summary: "connection_failed",
    checks: [
      { stage: "connection", status: "failed", reason: "timeout", vantage: "control_plane", target: "node.example:18800", source: "tcp_probe", observed_at: "2026-11-09T09:59:58Z" },
      { stage: "authentication", status: "unavailable", reason: "authentication_not_checked", vantage: "control_plane" },
      { stage: "accelerators", status: "unavailable", reason: "requires_agent", vantage: "node" },
    ],

};

test("offline doctor renders unavailable checks separately from connection failure", () => {
  render(<I18nextProvider i18n={zh}><NodeDoctorReport report={report} /></I18nextProvider>);
  const connection = screen.getByRole("region", { name: "连接建立" });
  assert.ok(within(connection).getByText("失败"));
  assert.ok(within(connection).getByText("node.example:18800"));
  assert.ok(within(connection).getByText(/尚不能确定阻断位置/));
  const auth = screen.getByRole("region", { name: "身份认证" });
  assert.ok(within(auth).getByText("无法检查"));
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
  assert.ok(screen.getAllByText("Unavailable").length === 2);
  assert.ok(screen.getByText(/blocking location is unknown/));
  view.rerender(<I18nextProvider i18n={en}><NodeDoctorReport report={{ report: "legacy result" }} /></I18nextProvider>);
  assert.ok(screen.getByText("legacy result"));
  assert.equal(screen.queryByRole("region", { name: "Authentication" }), null);
});

test("rerun replaces prior failures with current connection evidence", () => {
  const view = render(<I18nextProvider i18n={zh}><NodeDoctorReport report={report} /></I18nextProvider>);
  const next: Report = { ...report, report: "", summary: "completed", checks: [
    { stage: "connection", status: "passed", reason: "tcp_connected", vantage: "control_plane", target: "node.example:18800", source: "tcp_probe" },
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
  const current: Report = { ...report, report: "", summary: "completed", checks: [
    { stage: "connection", status: "passed", reason: "http_connected", vantage: "node", target: "http://server:8800/healthz", source: "http_probe" },
    { stage: "authentication", status: "unavailable", reason: "authentication_not_checked", vantage: "node" },
  ] };
  render(<I18nextProvider i18n={zh}><NodeDoctorReport report={current} /></I18nextProvider>);
  const connection = screen.getByRole("region", { name: "连接建立" });
  assert.ok(within(connection).getByText("通过"));
  assert.ok(within(connection).getByText("计算节点可访问管理节点的健康检查接口"));
  assert.ok(within(connection).getByText("本次节点侧 HTTP 探测"));
  assert.ok(within(screen.getByRole("region", { name: "身份认证" })).getByText("无法检查"));
});

test("one check list renders Agent findings with evidence and preserves hardware details", () => {
  const finding: NonNullable<Report["findings"]>[number] = {
    code: "runtime_provision_failed", level: "error", probe: "runtime setup", evidence: [{ name: "error", value: "permission denied" }], recheck: "check runtime permissions",
  };
  const current: Report = {
    report: "original report", checked_at: report.checked_at, summary: "failed",
    findings: [finding],
    checks: [
      { stage: "host", status: "info", reason: "information_collected", vantage: "node", source: "agent_rpc" },
      { stage: "finding", status: "failed", reason: "runtime_provision_failed", vantage: "node", source: "agent_rpc", finding },
    ],
    host: { hostname: "test-node", os: "linux", arch: "amd64", kernel: "test-kernel", cpu_model: "test-cpu", cpu_cores: 8, mem_total_bytes: 1024, mem_avail_bytes: 512, disks: [] },
  };
  const view = render(<I18nextProvider i18n={zh}><NodeDoctorReport report={current} /></I18nextProvider>);
  assert.ok(within(screen.getByRole("region", { name: "主机与磁盘检查" })).getByText("已获取"));
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
