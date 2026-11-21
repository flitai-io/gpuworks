# Network Monitoring Metrics

This page is a dictionary of node network and NIXL transfer metrics: their names, labels, measurement scope, and locations in the console. For network configuration, see [Configure Node Networks](../how-to/configure-networks.md). For general metric queries and log searches, see [View Metrics and Search Logs](../how-to/monitor-and-logs.md).

## Collection Method

Every 10 seconds, the agent reads counters from `/proc/net/dev` and sysfs without starting a process. It calculates per-second rates by dividing the difference between consecutive readings by the interval. The first round establishes a baseline without emitting data points; a round in which a counter wraps around or resets counts as 0. All `gpuworks_node_*` metrics carry a `node` label; the tables below list only the other labels.

Only physical NICs, bonds, bridges, and VLANs are included. Loopback interfaces and virtual interfaces created by containers or virtualization are excluded. Only RDMA ports in the ACTIVE state are included.

## Node Network Metrics

| Metric | Labels | Description |
|---|---|---|
| `gpuworks_node_nic_{rx,tx}_bytes_per_sec` | `iface` | Receive and transmit byte rates for each NIC (physical NIC, bond, bridge, or VLAN) |
| `gpuworks_node_nic_errors_per_sec` | `iface`, `kind` | NIC errors and dropped packets; `kind` is `rx_errors` / `rx_dropped` / `tx_errors` / `tx_dropped`. Not reported when the rate is 0; points are emitted only when the counters change |
| `gpuworks_node_rdma_{rx,tx}_bytes_per_sec` | `hca` (for example, `mlx5_0:1`) | Receive and transmit byte rates for ACTIVE RDMA ports, calculated by multiplying `port_rcv_data` / `port_xmit_data` (in units of 4 bytes) by 4 |
| `gpuworks_node_rdma_events_per_sec` | `hca`, `counter` | RDMA error counters (`symbol_error`, `link_downed`, `port_rcv_errors`, `port_xmit_discards`, `excessive_buffer_overrun_errors`) and RoCE congestion counters (`np_cnp_sent`, `rp_cnp_handled`, `out_of_buffer`). Not reported when the rate is 0; points are emitted only when the counters change |
| `gpuworks_node_role_{rx,tx}_bytes_per_sec` | `role` | Node traffic by role, measured from the node's perspective: `storage_sync` is counted by the agent when sending and receiving peer sync traffic; `serving` is counted by the server gateway on connections to the node, including both direct serving network connections and agent tunnels |

Measurement scope:

- Compute traffic over Ethernet cannot be separated from NIC counters and is included in NIC receive/transmit traffic. For host RDMA compute traffic, use RDMA port counters. Traffic through accelerator network ports does not pass through the host NIC and is not currently collected; the monitoring page for an accelerator RDMA network reflects only the bootstrap NIC.
- Role traffic includes all traffic for that role on the node, regardless of which network carries it.

## NIXL Transfer Metrics

When NIXL (PD disaggregation) is enabled for a vLLM instance, the following additional inference metrics are collected by renaming vLLM's native `vllm:nixl_*` metrics:

| Metric | Type | Description |
|---|---|---|
| `gpuworks_infer_nixl_transfer_seconds` | Histogram | Duration of a single KV transfer; `_count` is the number of transfers |
| `gpuworks_infer_nixl_transfer_bytes` | Histogram | Bytes in a single transfer; `_sum` is the cumulative number of bytes transferred |
| `gpuworks_infer_nixl_failed_transfers_total` | Counter | Number of failed transfers, including handshake and notification failures |
| `gpuworks_infer_nixl_kv_expired_total` | Counter | Number of requests whose KV data expires before it is retrieved |

For PD disaggregation deployment and troubleshooting, see [PD Disaggregation Deployment](../how-to/pd-disaggregation.md).

## Where to View Metrics in the Console

- The NIC inventory and RDMA port table on the node details **Network** page show real-time receive/transmit rates from the last two minutes.
- The **Monitoring** tab on a network details page (including the management network) charts, by node, the network's NIC receive/transmit rates, RDMA receive/transmit rates (for RDMA networks, summing multiple HCAs on the same node), errors and congestion, and node traffic for the storage sync and serving roles carried by that network. You can select a time range from 15 minutes to 24 hours.

<!-- translation-source-sha256: b389c70afee696a71c4c25736933dbd9a554f538ee10e3b0f6a9b44f111f1fd7 # Network Monitoring Metrics -->
<!-- translation-source-sha256: b0ea4a2129c36272312220c0c6e8c4b3e97761dd7e4fe92b426a964dede6cd1f ## Collection Method -->
<!-- translation-source-sha256: 126149e967534b9f6e7ab69d55f6ba377fbec325ff62c939244e5d7c2494050b ## Node Network Metrics -->
<!-- translation-source-sha256: e242f6c77419f2840bcaf32b765571556a657e6a75915331066a0af2dc0496f1 ## NIXL Transfer Metrics -->
<!-- translation-source-sha256: a9ce0bc788e5bfa0d67600d9e2ae8050a17256e4950a754745c744ae8bd245e0 ## Where to View Metrics in the Console -->
