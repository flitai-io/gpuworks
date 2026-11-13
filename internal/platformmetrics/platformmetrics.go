// Package platformmetrics is the vocabulary of the time series the platform
// measures itself: GPUs and dGPU slices, hosts and their networks, the KV
// disk tier, benchmark points and the gateway's operational counters. It sits
// beside inferencemetrics, which names what engines and the gateway report
// about inference; together they are every series the server stores and the
// catalog GET /api/v1/monitor/metrics answers.
//
// Like inferencemetrics it is a leaf: the agent's collectors and the server
// share it, so it imports nothing from the rest of the repository. A series
// a producer writes, or the server or UI queries, must be listed here or in
// inferencemetrics; TestEveryMetricLiteralIsRegistered enforces that.
package platformmetrics

import "sort"

// Source groups series by what measures them.
type Source string

const (
	SourceGPU          Source = "gpu"           // the agent's GPU probes (nvidia-smi, DCGM and the vendor tools)
	SourceDGPU         Source = "dgpu"          // a dGPU slice's own accounting
	SourceNode         Source = "node"          // the host: CPU, memory, disks
	SourceNetwork      Source = "network"       // the host's interfaces, NICs and RDMA ports
	SourceKVDisk       Source = "kv_disk"       // an engine's KV cache disk tier
	SourceBenchmark    Source = "benchmark"     // benchmark points, written by the server
	SourcePeerTransfer Source = "peer_transfer" // server-coordinated peer transfers
	SourceGateway      Source = "gateway"       // the gateway's operational counters
)

// Kind is how a value behaves over time.
type Kind string

const (
	Gauge   Kind = "gauge"   // a level, read as is
	Counter Kind = "counter" // monotonically increasing; read as a rate
)

// Definition is one stored series.
type Definition struct {
	Series string
	Source Source
	Kind   Kind
	// Unit is what one unit of the value is.
	Unit string
	// Labels are the label keys every sample carries (some samples carry
	// more, such as deployment on per-instance series). Series the server
	// writes itself carry node="server".
	Labels []string
	// Description says what the value measures, in English.
	Description string
}

var gpuLabels = []string{"node", "gpu", "gpu_uuid", "vendor"}

var definitions = []Definition{
	{"gpuworks_peer_relay_active", SourcePeerTransfer, Gauge, "transfers", []string{"node"}, "Active relay leases in this server process."},
	{"gpuworks_peer_relay_rate_waiting", SourcePeerTransfer, Gauge, "transfers", []string{"node"}, "Active relay transfers currently waiting for byte budget."},
	{"gpuworks_peer_relay_rate_limit_bytes_per_second", SourcePeerTransfer, Gauge, "bytes_per_second", []string{"node"}, "Server process aggregate encrypted relay byte limit."},
	{"gpuworks_peer_relay_bytes_total", SourcePeerTransfer, Counter, "bytes", []string{"node", "direction"}, "Actual encrypted bytes written by this server, counted once per direction."},
	{"gpuworks_peer_relay_rate_wait_seconds_total", SourcePeerTransfer, Counter, "seconds", []string{"node"}, "Union of concurrent byte-budget wait times per relay, summed across transfers."},
	{"gpuworks_peer_transfers_total", SourcePeerTransfer, Counter, "transfers", []string{"node", "outcome"}, "Peer copy attempts by coordinator outcome; peer_confirmed requires validated archive confirmation."},
	{"gpuworks_peer_probes_total", SourcePeerTransfer, Counter, "probes", []string{"node", "outcome"}, "Connectivity probes, separate from archive transfers."},
	{"gpuworks_gpu_utilization", SourceGPU, Gauge, "percent", gpuLabels, "Share of time the GPU's compute engines were busy over the last sample period (0-100)."},
	{"gpuworks_gpu_memory_used_bytes", SourceGPU, Gauge, "bytes", gpuLabels, "Device memory in use on the GPU, by every process."},
	{"gpuworks_gpu_memory_total_bytes", SourceGPU, Gauge, "bytes", gpuLabels, "Device memory the GPU has."},
	{"gpuworks_gpu_memory_bandwidth_utilization", SourceGPU, Gauge, "percent", gpuLabels, "Share of time device memory was being read or written (0-100)."},
	{"gpuworks_gpu_temperature_celsius", SourceGPU, Gauge, "celsius", gpuLabels, "GPU core temperature."},
	{"gpuworks_gpu_power_watts", SourceGPU, Gauge, "watts", gpuLabels, "GPU power draw."},
	{"gpuworks_gpu_fan_speed_percent", SourceGPU, Gauge, "percent", gpuLabels, "GPU fan speed as a share of its maximum; absent on passively cooled cards."},
	{"gpuworks_gpu_sm_clock_mhz", SourceGPU, Gauge, "megahertz", gpuLabels, "Current SM clock."},
	{"gpuworks_gpu_sm_clock_max_mhz", SourceGPU, Gauge, "megahertz", gpuLabels, "Maximum SM clock; a current clock well below it with throttle reasons set means the GPU is being held back."},
	{"gpuworks_gpu_throttle_reasons", SourceGPU, Gauge, "bitmask", gpuLabels, "nvidia-smi clock event (throttle) reasons as a bitmask; 0 means none."},
	{"gpuworks_gpu_sm_active", SourceGPU, Gauge, "ratio", gpuLabels, "DCGM: fraction of time at least one warp was active on an SM, averaged over SMs (0-1)."},
	{"gpuworks_gpu_sm_occupancy", SourceGPU, Gauge, "ratio", gpuLabels, "DCGM: fraction of resident warps relative to the SM maximum (0-1)."},
	{"gpuworks_gpu_tensor_active", SourceGPU, Gauge, "ratio", gpuLabels, "DCGM: fraction of cycles the tensor cores were active (0-1)."},
	{"gpuworks_gpu_pcie_rx_bytes_per_sec", SourceGPU, Gauge, "bytes_per_second", gpuLabels, "PCIe traffic into the GPU."},
	{"gpuworks_gpu_pcie_tx_bytes_per_sec", SourceGPU, Gauge, "bytes_per_second", gpuLabels, "PCIe traffic out of the GPU."},

	{"gpuworks_dgpu_memory_used_bytes", SourceDGPU, Gauge, "bytes", []string{"node", "gpu", "instance"}, "Device memory a dGPU slice's instance uses on one GPU, as the slice accounts it."},
	{"gpuworks_dgpu_sm_util", SourceDGPU, Gauge, "percent", []string{"node", "gpu", "instance"}, "SM utilization a dGPU slice's instance gets on one GPU (0-100)."},

	{"gpuworks_node_cpu_busy_percent", SourceNode, Gauge, "percent", []string{"node"}, "Share of CPU time the host spent not idle, over all cores (0-100)."},
	{"gpuworks_node_mem_used_bytes", SourceNode, Gauge, "bytes", []string{"node"}, "Host memory in use (total minus available)."},
	{"gpuworks_node_mem_total_bytes", SourceNode, Gauge, "bytes", []string{"node"}, "Host memory."},
	{"gpuworks_node_disk_read_bytes_per_sec", SourceNode, Gauge, "bytes_per_second", []string{"node", "device"}, "Reads from one block device."},
	{"gpuworks_node_disk_write_bytes_per_sec", SourceNode, Gauge, "bytes_per_second", []string{"node", "device"}, "Writes to one block device."},

	{"gpuworks_node_net_rx_bytes_per_sec", SourceNetwork, Gauge, "bytes_per_second", []string{"node"}, "Traffic received over all the host's interfaces."},
	{"gpuworks_node_net_tx_bytes_per_sec", SourceNetwork, Gauge, "bytes_per_second", []string{"node"}, "Traffic sent over all the host's interfaces."},
	{"gpuworks_node_nic_rx_bytes_per_sec", SourceNetwork, Gauge, "bytes_per_second", []string{"node", "iface"}, "Traffic received on one NIC."},
	{"gpuworks_node_nic_tx_bytes_per_sec", SourceNetwork, Gauge, "bytes_per_second", []string{"node", "iface"}, "Traffic sent on one NIC."},
	{"gpuworks_node_nic_errors_per_sec", SourceNetwork, Gauge, "events_per_second", []string{"node", "iface", "kind"}, "Errors or drops of one kind on one NIC."},
	{"gpuworks_node_rdma_rx_bytes_per_sec", SourceNetwork, Gauge, "bytes_per_second", []string{"node", "hca"}, "Traffic received on one RDMA port."},
	{"gpuworks_node_rdma_tx_bytes_per_sec", SourceNetwork, Gauge, "bytes_per_second", []string{"node", "hca"}, "Traffic sent on one RDMA port."},
	{"gpuworks_node_rdma_events_per_sec", SourceNetwork, Gauge, "events_per_second", []string{"node", "hca", "counter"}, "One RDMA port error or congestion counter's rate (counter names it)."},
	{"gpuworks_node_role_rx_bytes_per_sec", SourceNetwork, Gauge, "bytes_per_second", []string{"node", "role"}, "Traffic received on the network a role (serving, management, storage...) is bound to."},
	{"gpuworks_node_role_tx_bytes_per_sec", SourceNetwork, Gauge, "bytes_per_second", []string{"node", "role"}, "Traffic sent on the network a role is bound to."},

	{"gpuworks_kv_disk_used_bytes", SourceKVDisk, Gauge, "bytes", []string{"node", "instance", "engine"}, "Space an instance's KV cache disk tier uses."},
	{"gpuworks_kv_disk_limit_bytes", SourceKVDisk, Gauge, "bytes", []string{"node", "instance", "engine"}, "Space an instance's KV cache disk tier may use."},

	{"gpuworks_bench_output_tput", SourceBenchmark, Gauge, "tokens_per_second", []string{"benchmark", "point", "node"}, "A benchmark point's output token throughput."},
	{"gpuworks_bench_total_tput", SourceBenchmark, Gauge, "tokens_per_second", []string{"benchmark", "point", "node"}, "A benchmark point's input plus output token throughput."},
	{"gpuworks_bench_request_tput", SourceBenchmark, Gauge, "requests_per_second", []string{"benchmark", "point", "node"}, "A benchmark point's completed requests per second."},
	{"gpuworks_bench_ttft_p50_ms", SourceBenchmark, Gauge, "milliseconds", []string{"benchmark", "point", "node"}, "A benchmark point's median time to first token."},
	{"gpuworks_bench_tpot_p50_ms", SourceBenchmark, Gauge, "milliseconds", []string{"benchmark", "point", "node"}, "A benchmark point's median time per output token."},
	{"gpuworks_bench_latency_p99_ms", SourceBenchmark, Gauge, "milliseconds", []string{"benchmark", "point", "node"}, "A benchmark point's 99th percentile request latency."},

	{"gpuworks_proxy_routed_total", SourceGateway, Counter, "requests", []string{"deployment", "instance", "node"}, "Requests the gateway routed to each replica."},
	{"gpuworks_proxy_retries_total", SourceGateway, Counter, "requests", []string{"deployment", "reason", "node"}, "Requests the gateway retried on another replica, by why."},
	{"gpuworks_proxy_pd_fallback_total", SourceGateway, Counter, "requests", []string{"deployment", "reason", "node"}, "Prefill/decode requests the gateway served without disaggregation after the prefill step failed."},
	{"gpuworks_proxy_pd_prefill_duration_ms_sum", SourceGateway, Counter, "milliseconds", []string{"deployment", "node"}, "Total time spent in the prefill step of prefill/decode requests; with _count, the mean."},
	{"gpuworks_proxy_pd_prefill_duration_ms_count", SourceGateway, Counter, "requests", []string{"deployment", "node"}, "Prefill/decode requests whose prefill step was timed."},
	{"gpuworks_proxy_accounting_dropped_total", SourceGateway, Counter, "events", []string{"node"}, "Usage accounting records the gateway dropped because its queue was full."},
}

var bySeries = func() map[string]Definition {
	index := make(map[string]Definition, len(definitions))
	for _, d := range definitions {
		index[d.Series] = d
	}
	return index
}()

// Definitions returns the vocabulary sorted by series name.
func Definitions() []Definition {
	out := append([]Definition(nil), definitions...)
	sort.Slice(out, func(i, j int) bool { return out[i].Series < out[j].Series })
	return out
}

// Lookup reports the definition of a stored series.
func Lookup(series string) (Definition, bool) {
	d, ok := bySeries[series]
	return d, ok
}
