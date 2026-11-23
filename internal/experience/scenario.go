// Package experience runs the experience environment: a console of its own
// (own data directory, own port) holding simulated nodes and data, so a
// visitor sees how the product looks in use. It is for showing, not for
// operating: what would act on the real host, or would need a real cloud,
// network or storage, is refused with a message (guard.go).
//
// The console is the real control plane. The nodes are the release's own
// agent binaries run in simulation mode (internal/agent/simulate), one
// process per node; this process answers for their engines (engines.go),
// seeds the models, deployments, evaluation and usage history, and keeps a
// light stream of requests going.
package experience

import (
	"encoding/json"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/simulate"
)

// node is one simulated machine and the models already on its disk, by
// HuggingFace repository id.
type node struct {
	simulate.Spec
	Models []string
}

// scenario is the fleet the environment starts with: three NVIDIA
// generations, two domestic PPU servers and an Ascend server. Every model is
// on each node that can serve it, so a deployment never has to fetch one.
var scenario = []node{
	{Spec: server("gpu-h100-01", "10.20.0.11", nvidia("NVIDIA H100 80GB HBM3", 8, 81559, "9.0", 700),
		"Intel(R) Xeon(R) Platinum 8480+", 112, 2048, "amd64"),
		Models: []string{"Qwen/Qwen3-8B", "Qwen/Qwen3-32B", "Qwen/Qwen3.8-27B"}},
	{Spec: server("gpu-a100-01", "10.20.0.12", nvidia("NVIDIA A100-SXM4-80GB", 8, 81920, "8.0", 400),
		"AMD EPYC 7763 64-Core Processor", 128, 1024, "amd64"),
		Models: []string{"Qwen/Qwen3-8B", "Qwen/Qwen3-32B", "Qwen/Qwen3.8-27B", "Qwen/Qwen3-0.6B", "Qwen/Qwen3-1.7B"}},
	{Spec: server("gpu-4090-01", "10.20.0.13", nvidia("NVIDIA GeForce RTX 4090", 4, 24564, "8.9", 450),
		"Intel(R) Xeon(R) Gold 6430", 64, 512, "amd64"),
		Models: []string{"Qwen/Qwen3-8B"}},
	{Spec: server("ppu-zw810e-01", "10.20.0.15", ppu(), "Hygon C86 7390 32-core Processor", 128, 1024, "amd64"),
		Models: []string{"Qwen/Qwen3.8-27B"}},
	{Spec: server("ppu-zw810e-02", "10.20.0.16", ppu(), "Hygon C86 7390 32-core Processor", 128, 1024, "amd64"),
		Models: []string{"Qwen/Qwen3.8-27B"}},
	{Spec: server("npu-910b-01", "10.20.0.14", simulate.Spec{
		Vendor: "ascend", Framework: "cann", FrameworkVersion: "9.0.0", Driver: "24.1.rc3",
		Card: "Ascend 910B3", Count: 8, VRAMMiB: 65536, TDP: 350,
	}, "Kunpeng-920", 192, 1536, "arm64"),
		Models: []string{"Qwen/Qwen3-8B", "Qwen/Qwen3-32B"}},
}

func nvidia(card string, count int, vramMiB int64, cc string, tdp float64) simulate.Spec {
	return simulate.Spec{Vendor: "nvidia", Framework: "cuda", FrameworkVersion: "13.0", Driver: "570.172.08",
		Card: card, Count: count, VRAMMiB: vramMiB, ComputeCapability: cc, TDP: tdp}
}

func ppu() simulate.Spec {
	return simulate.Spec{Vendor: "ppu", Framework: "hggc", FrameworkVersion: "13.0", Driver: "1.5.2",
		Card: "PPU-ZW810E", Count: 8, VRAMMiB: 98304, ArchID: "Beethoven", TDP: 400}
}

func server(name, address string, cards simulate.Spec, cpu string, cores int, memGiB int64, arch string) simulate.Spec {
	cards.Hostname, cards.Address = name, address
	cards.OS, cards.Arch, cards.Kernel = "linux", arch, "5.15.0-125-generic"
	cards.CPU, cards.Cores, cards.MemGiB = cpu, cores, memGiB
	return cards
}

// reply is what every simulated engine answers: who it is, and that the
// answer is canned, so nobody mistakes the environment for real inference.
func reply(model string, messages json.RawMessage) string {
	name := model
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
		// The deploy form names a service "<org>/<model>-<4 random>"; the
		// visitor knows it by the model.
		if j := strings.LastIndex(name, "-"); j > 0 && len(name)-j == 5 && strings.ToLower(name[j:]) == name[j:] {
			name = name[:j]
		}
	}
	if name == "" {
		name = "GPUWorks"
	}
	question := lastUserMessage(messages)
	if question == "" {
		return fmt.Sprintf("你好，我是 %s 模型，是你的 AI 助理。当前是 GPUWorks 体验环境的模拟回复，没有进行真实推理。", name)
	}
	return fmt.Sprintf("你好，我是 %s 模型，是你的 AI 助理。\n\n你的问题是：「%s」\n\n当前是 GPUWorks 体验环境，回复为模拟数据，没有进行真实推理。部署到真实 GPU 节点后，这里会是模型生成的回答。", name, truncateRunes(question, 60))
}

func lastUserMessage(raw json.RawMessage) string {
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &messages) != nil {
		return ""
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(messages[i].Content, &text) == nil {
			return strings.TrimSpace(text)
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(messages[i].Content, &parts) == nil {
			for _, part := range parts {
				if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
					return strings.TrimSpace(part.Text)
				}
			}
		}
		return ""
	}
	return ""
}

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
