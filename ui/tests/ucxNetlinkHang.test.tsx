import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const zh = (await createI18n(["nodes", "deploy", "common"], "zh")).i18n;
const en = (await createI18n(["nodes", "deploy", "common"], "en")).i18n;
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: UCXNetlinkHangNotice, UCXNetlinkHangTag } = await import("../src/components/NodeUCXNetlinkHang.tsx");
const { default: PdUCXNetlinkFixes } = await import("../src/pages/deploy/PdUCXNetlinkFixes.tsx");
const { default: NodeDoctorReport } = await import("../src/components/NodeDoctorReport.tsx");
type Node = import("../src/api.ts").Node;
type EvaluateResponse = import("../src/api.ts").EvaluateResponse;

afterEach(() => cleanup());

const wrap = (node: React.ReactNode, i18n = zh) => render(<I18nextProvider i18n={i18n}>{node}</I18nextProvider>);

const NOW = "sudo sysctl -w net.ipv6.conf.all.disable_ipv6=1 net.ipv6.conf.default.disable_ipv6=1";
const PERSIST = "printf 'net.ipv6.conf.all.disable_ipv6=1\\nnet.ipv6.conf.default.disable_ipv6=1\\n' | sudo tee /etc/sysctl.d/90-gpuworks-ucx.conf && sudo sysctl --system";
const fixes = [
  { code: "disable_ipv6_now", commands: [NOW] },
  { code: "disable_ipv6_persist", commands: [PERSIST] },
];
const hanging = { ucx_netlink_hang: { kernel: "5.15.0-203-generic", fixes } } as Pick<Node, "ucx_netlink_hang">;

let copied = "";
Object.defineProperty(globalThis.navigator, "clipboard", { value: { writeText: async (text: string) => { copied = text; } }, configurable: true });

// The server decides a node hangs UCX and writes the commands; the page words
// the label and the why, and shows and copies the commands as received.
test("a node that hangs UCX says so in Chinese and offers the server's commands to copy", async () => {
  wrap(<><UCXNetlinkHangTag node={hanging} /><UCXNetlinkHangNotice node={hanging} /></>);
  assert.ok(screen.getAllByText("UCX/NIXL 在此节点会卡住（PD 分离不可用）").length >= 2, "the tag and the notice title");
  const alert = screen.getByRole("alert");
  assert.match(alert.textContent ?? "", /UCX 1\.20-1\.22/);
  assert.match(alert.textContent ?? "", /内核：5\.15\.0-203-generic/);
  assert.match(alert.textContent ?? "", /整台主机不再使用 IPv6/);
  assert.equal(screen.getByLabelText("disable_ipv6_now").textContent, NOW);
  assert.equal(screen.getByLabelText("disable_ipv6_persist").textContent, PERSIST);
  assert.ok(screen.getByText("立即关闭整机 IPv6（重启前有效）："));
  const buttons = screen.getAllByRole("button", { name: "复制命令" });
  assert.equal(buttons.length, 2);
  fireEvent.click(buttons[1]);
  await waitFor(() => assert.equal(copied, PERSIST));
  fireEvent.click(buttons[0]);
  await waitFor(() => assert.equal(copied, NOW));
});

test("the English notice says the same, and a node without the fact shows nothing", () => {
  wrap(<UCXNetlinkHangNotice node={hanging} />, en);
  assert.match(screen.getByRole("alert").textContent ?? "", /UCX\/NIXL hangs on this node \(PD disaggregation unavailable\)/);
  assert.ok(screen.getByText("Turn IPv6 off on the whole host until the next reboot:"));
  cleanup();
  const { container } = wrap(<><UCXNetlinkHangTag node={{}} /><UCXNetlinkHangNotice node={{}} /></>);
  assert.equal(container.textContent, "");
});

test("the notice without a kernel release and the tag are reachable by keyboard", () => {
  const unreadable = { ucx_netlink_hang: { fixes } } as Pick<Node, "ucx_netlink_hang">;
  wrap(<><UCXNetlinkHangTag node={unreadable} /><UCXNetlinkHangNotice node={unreadable} /></>);
  assert.doesNotMatch(screen.getByRole("alert").textContent ?? "", /内核：/);
  const tag = screen.getByLabelText(/UCX\/NIXL 在此节点会卡住（PD 分离不可用）: /);
  assert.equal(tag.getAttribute("tabindex"), "0");
  for (const pre of screen.getAllByLabelText(/disable_ipv6_/)) assert.equal(pre.getAttribute("tabindex"), "0");
});

const refused = (params: Record<string, unknown> | undefined, code = "PD_UCX_NETLINK_HANG"): EvaluateResponse => ({
  ok: true,
  warnings: [{ code, params }],
}) as unknown as EvaluateResponse;

// A node the operator chose is not refused: the PD section warns, says the
// deployment can still be created, and lists the server's commands to copy.
test("the deploy form warns of PD_UCX_NETLINK_HANG and lists its commands to copy", async () => {
  wrap(<PdUCXNetlinkFixes result={refused({ names: "pd-a", mode: "vllm-nixl", nodes: [{ node_uuid: "pd-a", node_name: "pd-a", kernel: "k" }], fixes })} />);
  const region = within(screen.getByTestId("pd-ucx-netlink-fixes"));
  assert.match(region.getByRole("alert").textContent ?? "", /节点 pd-a 的内核会让 UCX 卡住.*vllm-nixl.*部署仍允许创建/);
  assert.ok(region.getByText("解除卡死（在该节点上执行）"));
  assert.equal(region.getByLabelText("disable_ipv6_now").textContent, NOW);
  fireEvent.click(region.getAllByRole("button", { name: "复制命令" })[0]);
  await waitFor(() => assert.equal(copied, NOW));
});

test("the deploy form shows nothing for other answers, and skips a malformed fix", () => {
  for (const result of [
    null,
    { ok: true } as unknown as EvaluateResponse,
    { ok: false, errors: [{ field: "placement.pd", code: "PD_UCX_NETLINK_HANG", params: { fixes } }] } as unknown as EvaluateResponse,
    refused({ fixes }, "PD_GPU_MODELS_DIFFER"),
    refused(undefined),
    refused({ fixes: "nope" }),
    refused({ fixes: [{ code: 3 }, null, { code: "x" }] }),
  ]) {
    const { container } = wrap(<PdUCXNetlinkFixes result={result} />);
    assert.equal(container.textContent, "");
    cleanup();
  }
  wrap(<PdUCXNetlinkFixes result={refused({ fixes: [{ code: "disable_ipv6_now", commands: [NOW, 7] }, null] })} />);
  assert.equal(screen.getByLabelText("disable_ipv6_now").textContent, NOW);
  cleanup();
  // Over host RDMA the softer warning carries the same commands.
  wrap(<PdUCXNetlinkFixes result={refused({ names: "pd-b", network: "fabric", fix: NOW, fixes }, "PD_UCX_NETLINK_HANG_RDMA")} />, en);
  assert.match(screen.getByRole("alert").textContent ?? "", /pd-b.*RDMA network fabric/);
  assert.equal(screen.getByLabelText("disable_ipv6_persist").textContent, PERSIST);
});

test("the diagnosis explains a ucx_netlink_hang finding with the fix commands in its evidence", () => {
  const report = {
    report: "== gpuworks agent doctor ==\n",
    host: { hostname: "h", os: "linux", arch: "amd64", kernel: "5.15.0-203-generic", cpu_model: "EPYC", cpu_cores: 8, mem_total_bytes: 1 << 30, mem_avail_bytes: 1 << 29, disks: [] },
    runtimes: [{ name: "docker", status: "ok", selected: true, command: "" }],
    findings: [{
      code: "ucx_netlink_hang", level: "warning", probe: "agent: RTM_GETROUTE dump (AF_INET6) over a NETLINK_ROUTE socket",
      evidence: [{ name: "kernel", value: "5.15.0-203-generic" }, { name: "fix", value: NOW }, { name: "fix_persist", value: PERSIST }],
      recheck: "sysctl net.ipv6.conf.all.disable_ipv6 net.ipv6.conf.default.disable_ipv6",
    }],
    facts: [],
  } as unknown as import("../src/api.ts").NodeDoctorReport;
  wrap(<NodeDoctorReport report={report} />, en);
  const findings = within(screen.getByRole("region", { name: "Action required" }));
  assert.ok(findings.getByText("UCX/NIXL hangs on this kernel"));
  assert.ok(findings.getByText(/Run the fix command in the evidence/));
  assert.ok(findings.getByText(NOW));
});

const { warningText } = await import("../src/pages/deploy/deployWarnings.ts");

// The server's params are what the texts interpolate: the Ethernet warning
// names the nodes and the mode and says creation goes ahead, the RDMA warning
// carries the first command as well.
test("the Ethernet and the RDMA warnings read well in both languages with the server's params", () => {
  const params = { mode: "vllm-nixl", network: "management", names: "pd-a, pd-b", fix: NOW, nodes: [], fixes };
  for (const [i18n, expected] of [[zh, /节点 pd-a, pd-b 的内核会让 UCX 卡住.*vllm-nixl.*部署仍允许创建.*关闭整机 IPv6/], [en, /The kernel of pd-a, pd-b makes UCX hang.*vllm-nixl.*can still be created.*IPv6 for the whole host/]] as const) {
    const text = warningText(i18n.getFixedT(null, "deploy"), { code: "PD_UCX_NETLINK_HANG", params });
    assert.match(text, expected);
    const warning = warningText(i18n.getFixedT(null, "deploy"), { code: "PD_UCX_NETLINK_HANG_RDMA", params });
    assert.ok(warning.includes(NOW) && warning.includes("pd-a, pd-b") && warning.includes("management"), warning);
  }
});
