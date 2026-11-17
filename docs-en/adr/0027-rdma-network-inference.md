# ADR 0027: Replace RDMA Network Selectors With Inference and Node Overrides; Prefer Host Test Tools, Then Built-In Tools

**Status**: Accepted
**Date**: 2026-09-28

## Context

[ADR 0030](0030-network-objects.md) gives RDMA networks the same selectors as Ethernet networks: bootstrap subnet, interface-name wildcards, and HCA wildcards. When creating a compute network on gpu3 on 2026-09-28, these selectors reveal three problems (see `artifacts/archive/2026-09-28-rdma-zero-config/evidence-2026-09-28-lab.md` for the record):

- With all selectors empty, HCA selection includes all active ports, but bootstrap remains empty without any warning. All internal tests fail within 3 milliseconds.
- You must understand the relationship between bootstrap, HCAs, RoCE v2 GIDs, and interface IPs to configure them correctly. Removing the address from the 1G management interface does not exclude it from the rails.
- The agent already reports the facts needed for inference: interface addresses and speeds, RDMA port status, link layers and corresponding interfaces, and the management-network address.

Tests on the same day establish the basis for inference: bootstrap need not use an RDMA interface (the H200 handshakes over `eth0` and achieves over 1.3 Tb/s across eight unaddressed IB rails); InfiniBand ports do not need IP addresses; RoCE ports without IPv4 addresses can only fall back to RoCE v1, whereas NCCL 2.21+ and UCX use RoCE v2.

## Decision

1. **RDMA networks no longer have HCA or interface-name wildcards** (migration 106 deletes `networks.hca` and clears `interface` on RDMA networks). The subnet remains, now meaning an optional bootstrap subnet.
2. **Inference rules have a single implementation**: `internal/server/netplan/rdma.go`. HCA selection uses active ports: choose IB if available; otherwise choose RoCE with IPv4 addresses, retaining only the group with the highest speed. Bootstrap resolution follows this order: node override → network bootstrap subnet (leave empty and warn if no match; do not fall back) → address of the first HCA interface → management-network address for IB without IPoIB.
3. **User changes are stored on node attachments; inference results are not persisted.** Empty `hcas` on an attachment means infer; nonempty means fixed. On RDMA networks, `interface`/`ip` specify a bootstrap override. The first HCA addition or removal writes and fixes the current list; "Restore Automatic" clears it. Removing an HCA applies by default to every node on the network that uses it, because rails must pair across nodes. The last HCA on a node cannot be removed. Modifying a node attached through labels creates a node attachment, retaining the rule that "nodes take precedence over labels." Resolution remains on demand (ADR 0030 decision 3): approximately 6 ms for 100 nodes.
4. **Internal tests report by rail**: rails are paired first by HCA IPs in the same subnet, with the remainder paired in order. Node pairs are classified as passed / partially passed / failed. Failures include stable reason codes and identify the affected node side, with messages localized by the client. RoCE ports explicitly use IPv4 RoCE v2 GIDs; if none is found, the test fails directly for that reason. A "ring" test mode is also available (N pairs, one round).
5. **Test tools are selected in this order: host `ib_write_bw`, agent-built-in `ib_write_bw`, then a prompt to install perftest.** The built-in version is built from upstream source (`build/runtime-assets/perftest/`), dynamically links the host's `libibverbs` and `librdmacm`, includes no vendor direct-verbs libraries, and disables the `ibv_wr_*` send interface.

## Consequences

- Creating an RDMA network requires no fields to be filled in. In two lab environments—Ascend (hns RoCE, point-to-point /30) and Nebius H200 (8×400G IB)—the rules infer the same results as correct manual configuration.
- Inference also selects storage RDMA ports with the same speed; you remove them from the node list. Nodes with fixed settings no longer automatically track newly appearing ports, relying on warnings such as `hca_unusable` and "Restore Automatic" as safeguards.
- The built-in tool covers only hosts with RDMA userspace libraries installed but without perftest. Hosts without the userspace libraries still need perftest installed (the package manager installs the libraries alongside it). The built-in version introduces a `libpci.so.3` dependency and an `IBVERBS_1.12` minimum (rdma-core 33+), enforced by `make check-perftest`.
- Deployments do not set NCCL's GID index: the RoCE v2 IPv4 GID automatically selected by NCCL 2.21+ matches the tests. Custom images with earlier versions must set `NCCL_IB_GID_INDEX` themselves.

<!-- translation-source-sha256: f539670be7c0277a886fbd0cc834b8f953b042905f21636d84a2f742481ebb9c -->
