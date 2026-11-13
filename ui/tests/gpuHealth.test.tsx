import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n } = await createI18n(["nodes"], "zh");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: GPUHealthTag, gpuHealthStatus } = await import("../src/components/GPUHealth.tsx");
const { default: NodeDoctorReport } = await import("../src/components/NodeDoctorReport.tsx");
type ReportT = import("../src/api.ts").NodeDoctorReport;

afterEach(() => cleanup());

const wrap = (node: React.ReactNode) => render(<I18nextProvider i18n={i18n}>{node}</I18nextProvider>);

const pendingGPU = {
  schedulable: true,
  health_status: "Warning",
  health_code: "memory_repair_pending",
  health_message: "remapped_rows.pending=Yes\nremapped_rows.uncorrectable=0",
  health_evidence: [{ name: "remapped_rows.pending", value: "Yes" }, { name: "remapped_rows.uncorrectable", value: "0" }],
  health_guide: {
    probe: "ppu-smi --query-ppu=pci.bus_id,remapped_rows.pending --format=csv -i 0000:7e:00.0",
    recheck: "ppu-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i 0000:7e:00.0",
    actions: ["ppu-smi -r -i 0000:7e:00.0"],
  },
};

test("a healthy card shows only its level, with no details button", () => {
  wrap(<GPUHealthTag gpu={{ schedulable: true, health_status: "OK" }} />);
  assert.ok(screen.getByText("正常"));
  assert.equal(screen.queryByRole("button", { name: "查看健康详情" }) === null, true);
});

test("a card waiting for a reset shows the short label and explains it on demand", async () => {
  wrap(<GPUHealthTag gpu={pendingGPU} />);
  assert.ok(screen.getByText("待复位"));
  assert.equal(screen.queryByText("remapped rows pending") === null, true, "the raw field never shows in the cell");
  fireEvent.click(screen.getByRole("button", { name: "查看健康详情" }));
  await waitFor(() => assert.ok(screen.getByText(/该卡存在待替换的显存行或显存页/)));
  assert.ok(screen.getByText("可正常使用；调度时优先选择其他健康的加速卡。"));
  for (const command of [pendingGPU.health_guide.probe, pendingGPU.health_guide.recheck, "ppu-smi -r -i 0000:7e:00.0"]) {
    assert.ok(screen.getByText(command), `missing command ${command}`);
  }
  const evidence = screen.getByRole("list");
  assert.ok(within(evidence).getByText("remapped_rows.pending"));
  assert.ok(within(evidence).getByText("(异常值)"), "the triggering value is marked");
  assert.equal(within(evidence).getAllByText("(异常值)").length, 1, "a zero counter is not marked");
  assert.ok(screen.getAllByRole("button", { name: "复制命令" }).length >= 3);
});

test("a failed replacement stops scheduling and names no command to run", async () => {
  wrap(<GPUHealthTag gpu={{
    schedulable: false, health_status: "Error", health_code: "memory_remap_failed",
    health_evidence: [{ name: "remapped_rows.failure", value: "Yes" }],
    health_guide: { probe: "ppu-smi --query-ppu=pci.bus_id,remapped_rows.failure --format=csv -i 0000:c6:00.0", recheck: "ppu-smi -q -d ROW_REMAPPER -i 0000:c6:00.0" },
  }} />);
  assert.ok(screen.getByText("显存故障"));
  fireEvent.click(screen.getByRole("button", { name: "查看健康详情" }));
  await waitFor(() => assert.ok(screen.getByText("已停止调度。")));
  assert.ok(screen.getByText("请联系硬件厂商。"));
  assert.equal(screen.queryByText(/ppu-smi -r/) === null, true);
});

test("a row from an older agent falls back to its level and raw sentence", async () => {
  wrap(<GPUHealthTag gpu={{ schedulable: true, health_status: "Warning", health_code: "ppu_pending_repair", health_message: "remapped rows pending" }} />);
  assert.ok(screen.getByText("告警"));
  fireEvent.click(screen.getByRole("button", { name: "查看健康详情" }));
  await waitFor(() => assert.ok(screen.getByText("remapped rows pending")));
  assert.ok(screen.getByText("代理报告了该状态，详见原始信息。"));
  assert.ok(screen.getByText("原始信息"));
});

test("gpuHealthStatus reads the level the server stored", () => {
  assert.equal(gpuHealthStatus({ health_status: "OK" }), "OK");
  assert.equal(gpuHealthStatus({ health_status: "Warning" }), "Warning");
  assert.equal(gpuHealthStatus({ health_status: "Error" }), "Error");
  assert.equal(gpuHealthStatus({ health_status: "Alarm" }), "Error");
  // No level is not a fault: the server states one for every card it owns.
  assert.equal(gpuHealthStatus({}), "OK");
});

// Somebody outside the platform is using the card: it is not faulty, and
// the operator is told who is on it rather than shown a red level.
test("a card in use outside the platform names the processes instead of a fault", async () => {
  wrap(<GPUHealthTag gpu={{
    schedulable: false, health_status: "OK",
    foreign_processes: [{ pid: 4242, name: "python3", memory_bytes: 8 * 1024 ** 3 }, { pid: 77, name: "Xorg" }],
  }} />);
  const tag = screen.getByText("被平台外的进程占用");
  assert.equal(screen.queryByText("故障") === null, true);
  assert.equal(screen.queryByText("正常") === null, true);
  const described = tag.closest("[aria-label]")?.getAttribute("aria-label") ?? "";
  assert.ok(described.includes("python3 (pid 4242, 8 GiB)"), described);
  assert.ok(described.includes("Xorg (pid 77)"), described);
  fireEvent.focus(tag.closest("[aria-label]") as HTMLElement);
  await waitFor(() => assert.match(screen.getByRole("tooltip").textContent ?? "", /python3 \(pid 4242, 8 GiB\)/));
});

const structured: ReportT = {
  report: "== gpuworks agent doctor ==\n",
  host: {
    hostname: "172-24-0-210", os: "linux", arch: "amd64", kernel: "4.19.90", cpu_model: "Xeon 8558P", cpu_cores: 192,
    mem_total_bytes: 2014 * 1024 ** 3, mem_avail_bytes: 1819 * 1024 ** 3,
    disks: [{ kind: "root", path: "/", total_bytes: 200 * 1024 ** 3, avail_bytes: 14 * 1024 ** 3 }],
  },
  runtimes: [
    { name: "docker", status: "ok", version: "26.1.3", selected: true, command: "docker version" },
    { name: "dcgmi", status: "not_found", command: "dcgmi --version" },
  ],
  accelerators: {
    vendor: "ppu", driver: "1.6.1-f3016a",
    gpus: [
      { index: 0, name: "PPU-ZW810E", memory_bytes: 96 * 1024 ** 3, pcie_bdf: "0001:c8:00.0", numa_node: 3, health_status: "OK" },
      { index: 5, name: "PPU-ZW810E", memory_bytes: 96 * 1024 ** 3, pcie_bdf: "0000:7e:00.0", numa_node: 0, health_status: "Warning", health_code: "memory_repair_pending" },
    ],
  },
  findings: [
    {
      code: "memory_repair_pending", level: "warning",
      targets: [{
        index: 5, pcie_bdf: "0000:7e:00.0",
        evidence: [{ name: "remapped_rows.pending", value: "Yes" }],
        probe: "ppu-smi --query-ppu=pci.bus_id,remapped_rows.pending --format=csv -i 0000:7e:00.0",
        recheck: "ppu-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i 0000:7e:00.0",
        actions: ["ppu-smi -r -i 0000:7e:00.0"],
        processes: [{ pid: 135707, name: "sglang::scheduler" }],
      }],
    },
    { code: "disk_low", level: "warning", disk: { kind: "root", path: "/", total_bytes: 200 * 1024 ** 3, avail_bytes: 14 * 1024 ** 3 }, probe: "statfs(/)", recheck: "df -h /" },
    { code: "multi_gpu_check_failed", level: "warning", probe: "torch.distributed all_reduce (backend nccl)", evidence: [{ name: "result", value: "FAIL: nccl timeout" }] },
  ],
  facts: [{ code: "device_injection", value: "explicit" }, { code: "runtime_plugin", value: "none" }],
};

test("the structured report is rendered in the reader's locale with evidence and recheck for every finding", () => {
  wrap(<NodeDoctorReport report={structured} />);
  assert.ok(screen.getByText("厂商 ppu · 驱动 1.6.1-f3016a · 共 2 张：1 张正常，1 张告警，0 张故障"));
  assert.ok(screen.getByText("0000:7e:00.0"));
  const findings = screen.getByRole("region", { name: "需要处理" });
  assert.ok(within(findings).getByText("1 张卡：5"));
  assert.ok(within(findings).getByText("卡上仍有进程 135707 sglang::scheduler，请先停止。"));
  assert.ok(within(findings).getByText("ppu-smi -r -i 0000:7e:00.0"));
  assert.ok(within(findings).getByText("ppu-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i 0000:7e:00.0"));
  // Disk: evidence and recheck, and deliberately no remedy.
  assert.ok(within(findings).getByText("磁盘空间不足"));
  assert.ok(within(findings).getByText("statfs(/)"));
  assert.ok(within(findings).getByText("df -h /"));
  assert.ok(within(findings).getByText("/ 可用 14.0 GiB，占总容量 200.0 GiB 的 7%。"));
  // A check with no single recheck command says so.
  assert.ok(within(findings).getByText("无对应的单条命令，请重新执行诊断。"));
  assert.ok(screen.getByText("部署时由 gpuworks 直接将加速卡设备挂载进容器，仅挂载可调度（正常与告警状态）的卡。"));
  assert.ok(screen.getByText("无（由 gpuworks 直接挂载设备）"));
  assert.ok(screen.getByText("已选用"));
  assert.ok(screen.getByText("未安装"));
});

test("a report with nothing to act on says so", () => {
  wrap(<NodeDoctorReport report={{ ...structured, findings: [] }} />);
  assert.ok(screen.getByText("未发现需要处理的问题。"));
});

test("a report from an older agent is shown verbatim with an upgrade hint", () => {
  wrap(<NodeDoctorReport report={{ report: "== gpuworks agent doctor ==\nhealth detail: remapped rows pending\n" }} />);
  assert.ok(screen.getByText(/该节点的代理版本较旧/));
  assert.ok(screen.getByText(/health detail: remapped rows pending/));
});

test("cards sharing a finding get its explanation once, and their own commands each", () => {
  const finding = structured.findings![0];
  const second = { ...finding.targets![0], index: 6, pcie_bdf: "0001:0b:00.0", processes: undefined, actions: ["ppu-smi -r -i 0001:0b:00.0"] };
  wrap(<NodeDoctorReport report={{ ...structured, findings: [{ ...finding, targets: [finding.targets![0], second] }] }} />);
  const findings = screen.getByRole("region", { name: "需要处理" });
  assert.equal(within(findings).getAllByText(/该卡存在待替换的显存行或显存页/).length, 1);
  assert.equal(within(findings).getAllByText("在卡空闲时执行以下命令复位；复位未成功时，重启主机。").length, 1);
  assert.ok(within(findings).getByText("ppu-smi -r -i 0000:7e:00.0"));
  assert.ok(within(findings).getByText("ppu-smi -r -i 0001:0b:00.0"));
  assert.ok(within(findings).getByText("加速卡 6（0001:0b:00.0）"));
});

// A runtime check's command carries the script it runs, which is dozens of
// lines. Shown in full it buries the evidence; truncated silently it would be
// pasted and fail. So it is collapsed, said to be collapsed, and the copy
// button still hands over the whole thing.
const runtimeUnusableGPU = {
  schedulable: false,
  health_status: "Error",
  health_code: "runtime_unusable",
  health_evidence: [{ name: "Runtime Check", value: "failed" }, { name: "Detail", value: "Ascend ACL runtime smoke failed: aclrtSetDevice=507033" }],
  health_guide: {
    probe: ["die='0'", "", "# the logical ACL device id of this die", "acl_id=\"$(npu-smi info -m | awk -v d=\"$die\" '...')\"", "python3 -c 'import ctypes, os'", "print(1)"].join("\n"),
    recheck: "npu-smi info -m; npu-smi info",
  },
};

test("a long runtime-check command is collapsed, and the whole of it is what gets copied", async () => {
  const copied: string[] = [];
  const clipboard = { writeText: async (text: string) => void copied.push(text) };
  Object.defineProperty(globalThis.navigator, "clipboard", { value: clipboard, configurable: true });

  wrap(<GPUHealthTag gpu={runtimeUnusableGPU} />);
  fireEvent.click(screen.getByRole("button", { name: "查看健康详情" }));

  const lines = runtimeUnusableGPU.health_guide.probe.split("\n");
  await waitFor(() => assert.ok(screen.getByText(/die='0'/), "the command's first line is shown"));
  // The tail is withheld until asked for, so the evidence stays readable.
  assert.equal(screen.queryByText(/print\(1\)/) === null, true, "the whole command is shown without being asked");
  const expand = screen.getByRole("button", { name: `展开全部 ${lines.length} 行` });

  // Copying does not depend on expanding: a reader who only wants to paste it
  // gets the command entire.
  fireEvent.click(screen.getAllByRole("button", { name: "复制命令" })[0]);
  await waitFor(() => assert.equal(copied[0], runtimeUnusableGPU.health_guide.probe));

  fireEvent.click(expand);
  await waitFor(() => assert.ok(screen.getByText(/print\(1\)/), "expanding shows the rest"));
  assert.ok(screen.getByRole("button", { name: "收起" }));
});

// The vendor tool's own view is short and stays whole: only a command that
// does not fit is collapsed.
test("a short command is shown in full with no expander", async () => {
  wrap(<GPUHealthTag gpu={runtimeUnusableGPU} />);
  fireEvent.click(screen.getByRole("button", { name: "查看健康详情" }));
  await waitFor(() => assert.ok(screen.getByText("npu-smi info -m; npu-smi info")));
  assert.equal(screen.queryByRole("button", { name: "收起" }) === null, true);
});


test("missing telemetry is a visible warning, not a reported CLI failure", () => {
  wrap(<GPUHealthTag gpu={{ schedulable: true, health_status: "Warning", health_code: "telemetry_unavailable", health_message: "ecc_errors.volatile.dram_uncorrectable=N/A" }} />);
  assert.equal(screen.queryByText("诊断数据缺失") !== null, true);
  assert.equal(screen.queryByText("不可读取") === null, true);
});


test("incomplete driver inventory is visible without disabling verified cards", async () => {
 wrap(<GPUHealthTag gpu={{ schedulable: true, health_status: "Warning", health_code: "inventory_incomplete", health_message: "attached_xpus=8\ninventory_rows=7" }} />);
 assert.equal(screen.queryByText("设备清单不完整") !== null, true);
 fireEvent.click(screen.getByRole("button", { name: "查看健康详情" }));
 await waitFor(() => assert.equal(screen.queryByText(/驱动报告的卡数与设备清单行数不一致/) !== null, true));
 assert.equal(screen.queryByText("已停止调度。") === null, true);
});
