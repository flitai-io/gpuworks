# Configure Node Networks

Specify which networks nodes with multiple interfaces use for storage sync, compute traffic (multi-node inference and PD KV transfer), and serving traffic, then use internal tests to check connectivity and bandwidth between nodes. Creating a network only declares the purpose of an existing network; it does not configure switches, interface addresses, or wiring.

Before you start, sign in with an administrator account and confirm that the relevant nodes are [connected](connect-nodes.md) and have reported their network inventories. Prepare for the network type you plan to use:

- **Host Ethernet**: Nodes already have usable addresses on the target network.
- **Host RDMA**: Nodes have ACTIVE InfiniBand or RoCE ports; the interfaces associated with RoCE ports also need IPv4 addresses. Bandwidth tests require a runnable `ib_write_bw`; see [Internal Tests](#内部测试) for tool requirements.
- **Accelerator RDMA**: The ports on Ascend cards have configured addresses and UP links, and the agent can read them with `hccn_tool`.

For deployment steps, see [Multi-Node Inference](multi-node-inference.md) and [PD Disaggregation](pd-disaggregation.md). For the design and inference rules of network objects, see [ADR 0030](../adr/0030-network-objects.md) and [ADR 0027](../adr/0027-rdma-network-inference.md).

## Listen Addresses and How Others Connect

The server neither stores nor infers its external address (see [ADR 0025](../adr/0025-server-holds-no-external-address.md)).
The **Listen addresses** card under Settings → General (the `listen.*` settings in `server.yaml`; see the [configuration reference](../reference/configuration.md#在控制台修改监听地址) for how to change them) only determines which `IP:port` addresses the server binds to **locally**. Callers choose whether to connect through a domain name, reverse proxy, or NAT forwarding.
UI fields that require a server address are prefilled with the address currently used by your browser. You can change it to a domain name or reverse proxy address; the corresponding API field, `server_url`, must be supplied explicitly.
Changing `listen.agent` affects **already installed** agents: they only dial the address in their installation command. If that address no longer forwards to the new listen address, they go offline until forwarding is fixed or the agents are reinstalled.

The **Network topology and settings** card at the top of **Compute → Networks** shows each setting on a simple topology diagram and a complex topology diagram,
alongside a numbered reference table explaining where you configure it, what it does, and what it does not determine. Hover over a number or table row to highlight the corresponding items.
If these address concepts are unfamiliar, start with this diagram.

## Networks

1. Open **Compute → Networks**, click **New network**, enter a name, select the roles and type, and configure selectors as described below. A network can carry multiple roles; Accelerator RDMA and Accelerator HCCS can only carry the Compute role.
2. Click **Save** to open the new network's details page. Attach it to the target nodes as described in [Attach to Nodes or Labels](#加载到节点或标签).
3. In the node list on the details page, check the actual IP addresses and interfaces; for RDMA networks, also check the bootstrap addresses and HCAs. An orange exclamation mark indicates configuration that is not in effect or is unavailable. Check the reason and correct it first.
4. To verify paths between nodes, run an [internal test](#内部测试). Successfully saving a network declaration does not mean the physical links work. Running instances continue to use the network they started with; changes apply the next time they start.

Fields used when creating a network:

- **Roles** (multiple selections allowed): **Storage sync** (synchronizes models and images between nodes), **Compute** (NCCL/Gloo for multi-node inference and KV transfer for PD disaggregation), and **Serving** (gateway access to inference instances). Select all three for an all-in-one network.
- **Type**: **Host Ethernet** (`host_ethernet`), **Host RDMA** (`host_rdma`), **Accelerator RDMA** (`accelerator_rdma`), or **Accelerator HCCS** (`accelerator_hccs`). Host RDMA covers both InfiniBand and RoCE. Each node's type is detected automatically from the link layer of its RDMA ports and shown on its row; an orange exclamation mark appears if different link layers are detected within the same network. Accelerator RDMA uses the RoCE ports built into accelerator cards (Ascend NPUs); Accelerator HCCS uses the HCCS/HIXL fabric on A3/910C; see [the sections below](#加速器-hccs-网络a3910c-灵衢链路). The type determines the NCCL/UCX/HCCL settings and device injection for the compute network. Only nodes on the same network object are considered mutually reachable.
- **Default selector** (required only for Host Ethernet): Enter a **Default CIDR**. Traffic uses an interface whose address falls within it. An interface name (supports the `*`, `?`, and `[...]` wildcards) is needed only when the subnet alone cannot distinguish interfaces.
- Host RDMA, Accelerator RDMA, and Accelerator HCCS networks **require no selectors**; see the next three sections. **Advanced** contains only an optional bootstrap CIDR.

### RDMA Networks: Automatic Bootstrap and HCA Inference

Each node on an RDMA network uses two things:

- **HCA** (rail): An RDMA port that carries data, such as `mlx5_0:1`. A RoCE port's **HCA IP** is the IPv4 address of its interface, which RoCE v2 uses to establish connections. InfiniBand ports do not need an IP and display “—”.
- **Bootstrap IP**: A TCP address for handshakes (NCCL out-of-band communication, Gloo, the vLLM master address, and ib_write_bw connection parameter exchange). Data does not pass through it. It does not have to be on the RDMA interface.

After nodes are attached, values are inferred for each node from the network inventory reported by its agent:

| | Rule |
|---|---|
| HCA | Select ACTIVE ports. If the node has InfiniBand ports, select only InfiniBand; otherwise, select RoCE. RoCE ports require an IPv4 address on their interface. Keep only the group with the highest speed (use interface speed for RoCE), excluding, for example, 1G management ports. |
| Bootstrap | Select in this order: the bootstrap specified on the node; if a bootstrap CIDR is set on the network, an interface whose address falls within it (leave it empty and warn if none matches; do not fall back); the address of the first HCA's interface; for InfiniBand without IPoIB, the management network address. |

The node list on the network details page shows each node's bootstrap IP and grouped HCAs with HCA IPs, marked **Auto** or **Manual**:

- Click ✕ next to an HCA to remove it. By default, this removes it from **every** node on the network that uses it (HCAs on each node must be paired one to one). You can select the option to remove it from this node only. **Add HCA** lets you select from the ports reported by that node; unavailable ports show the reason (inactive or no IPv4 address on the interface). After the first addition or removal, the node's HCA list is fixed to the list at that moment (**Manual**) and no longer changes with the inventory. **Back to auto** restores inference. You cannot remove a node's last HCA; detach the node if you no longer need it on the network.
- **Change** next to bootstrap lets you select an interface on that node that has an address; **Back to auto** clears the override.
- These changes are stored only in the node attachment. For a node attached through a label, the first change creates a node attachment for it.

Inferred nodes show the following warnings (orange exclamation marks): no usable RDMA port, no bootstrap address, an unavailable manually specified HCA, or inconsistent HCA counts across nodes.

### Accelerator RDMA Networks: Ports Built Into Accelerator Cards

Each Ascend 910 card has a built-in RoCE port that the host kernel cannot see (it is absent from both the interfaces and RDMA ports in the network inventory) and that can only be accessed with `hccn_tool`. Ascend collective communication across machines (HCCL) uses only these ports. There is no setting to redirect it to host interfaces, and it does not fall back automatically. Therefore, **Ascend multi-node deployments must use an Accelerator RDMA network**; otherwise, they are rejected (`COMPUTE_NETWORK_ACCELERATOR_REQUIRED`).

Each node on an Accelerator RDMA network also uses two things:

- **Rail**: An accelerator card, written as `npu3` (card 3). The agent reads its address, link state, and speed with `hccn_tool` and refreshes them every minute. The **Accelerator ports** table in **Network inventory** on the node details page lists all of them.
- **Bootstrap IP**: A host address for handshakes (HCCL communication domain setup, Gloo, and the vLLM master address). Host processes cannot bind to the address of an accelerator port, so this must be an address on a host interface.

| | Rule |
|---|---|
| Rail | Accelerator cards with an UP link and a configured IPv4 address, ordered by card number. |
| Bootstrap | Select in this order: the bootstrap specified on the node; if a bootstrap CIDR is set on the network, a host interface whose address falls within it; the management network address. |

Adding and removing rails, changing the bootstrap, and the meanings of **Auto / Manual** are the same as for Host RDMA networks. **Add HCA** lists the node's accelerator cards.

This type **can only carry the Compute role**: storage sync and serving run as host processes, which cannot bind to accelerator port addresses. These two roles are disabled in the form, and the API returns `NETWORK_TYPE_ROLE_INVALID`.

The network only declares that these nodes' accelerator ports are connected. It does not determine which cards an instance uses: the instance uses the cards selected by scheduling, and reachability between any two cards depends on the wiring. If two machines are connected directly by matching card numbers, only cards with the same number can reach each other. Addresses do not reveal this (cards on both machines can be on the same subnet).

### Accelerator HCCS Networks: A3/910C LingQu Fabric

An A3/910C node exposes its LingQu HCCS/HIXL fabric through the accelerator
cards' vNICs. The agent reads each card's vNIC address and link state with
`hccn_tool -vnic -g`, and validates a peer with
`hccn_tool -i <card-index> -hccs_ping -g address <peer IP>`. The cards are
still represented as accelerator rails, but an HCCS network selects the HCCS
protocol rather than the RoCE `-ping` command.

Each node uses the same card rail and host bootstrap concepts as Accelerator
RDMA. The network type is the fabric declaration; it does not require editing
`/etc/hccn.conf`, and the agent refreshes the observed card addresses and link
state automatically. HCCS networks can carry only Compute and are selected
automatically by deployment and card tests once the network is attached.

### Attach to Nodes or Labels

On the network details page, click **Attach**, select multiple nodes or switch to a node label and enter `key=value`, then click **Save**. Check the attachment results and node list. If individual nodes fail, address only those failures; you do not need to repeat successful attachments.

A network can be attached to any number of nodes and node labels. Each node ultimately uses only one network for each role:

1. Direct node attachments take precedence over label attachments.
2. If multiple networks for the same role match at the same priority, the earliest attachment takes effect. The node details page and the node list on the network details page show an orange exclamation mark beside that node; hover over it to see which networks are not in effect.

**Attach** lets you select multiple nodes at once. Failure for an individual node (for example, a deleted node) does not affect the others. Results are listed for each node, and nodes that are already attached count as successful.

When attaching a Host Ethernet network to a node, you can override the default selector by specifying an interface or IP. For Host RDMA, Accelerator RDMA, and Accelerator HCCS networks, you can specify a bootstrap interface or IP on the node (see the previous sections) and add or remove rails in the node list. Overrides are validated against the agent's reported network inventory; values absent from the inventory also show an orange exclamation mark. You can save even when the node is offline.

**Loaded networks** on the node details page lists the network, interface, IP, and HCAs actually used by this node for each role. **Network inventory** contains the interfaces, addresses, speeds, and RDMA ports the agent reads from sysfs, plus the accelerator ports read with `hccn_tool` on Ascend nodes. Only physical interfaces and virtual interfaces with addresses (bridges, bonds, and VLANs) are reported.

### Management Network

Every node is on the built-in **Management network**. Its address is the local address the node uses to connect to the server (when connecting through 127.0.0.1 or an HTTP proxy, the address of the default-route interface is used instead), reported automatically by the agent. The management network has no corresponding network object and cannot be deleted, modified, or attached; it appears as the first row in the network list. It carries any role without an attached network. On each node, you can separately disable the management network's permission to carry storage sync, compute traffic, or serving. All are enabled by default, and changes apply only to new instances (administrator access required):

- The node table on the management network details page has a switch column for each role, allowing individual changes. If a role is enabled but already carried by a dedicated network, the switch is marked **Carried by a dedicated network**.
- On the same page, search by name or address, filter by node label, and select nodes (or **Select filtered**) for a bulk edit. For each role, select **Keep / Allow / Deny**. Only the selected roles are changed, and the update succeeds or fails for all nodes together.
- You can also toggle the roles for a node in the **Management network** card on its **Networks** page. When the management network carries serving, traffic goes through the agent tunnel; the serving network forwarder is not enabled.

The **Management network** card also shows the connection source seen by the server. If it differs from the management network address, it is marked **Behind NAT**; this is informational only.

### How Each Role Takes Effect

**Storage sync: confirm the path used by both ends**

Model and image copies between nodes select a network per node pair: a shared storage-sync network takes precedence. Without one, the management network is used only if both ends allow it to carry storage sync. Automatic copying occurs only within the same [sync group](configure-pull-settings.md#同步组); manual sync is not restricted by sync groups.

The receiver listens only on the selected network's IP, and the sender connects from its own IP on that network. Among candidate sync sources, nodes that share a storage-sync network with the target appear first and are recommended.

If the two ends have no usable common path, no direct node-to-node connection is made. Automatic copying skips that source, and the replica pulls the image or downloads the model itself. Manually selecting that source returns `NO_STORAGE_PATH`.

When every direct candidate fails to establish a TCP connection, the server may relay models or images if both nodes allow **storage sync** on the management network and upgraded agents have independent data connections available. For upgraded agent-initiated connections and server-initiated enrollment over SSH, relay uses a data connection separate from the control connection. Private addresses need not be mutually reachable, and random ports need not be individually mapped. Older agents and nodes using a single connection support direct transfer only. Upgrade and enable independent data connections for relay; unavailable data connections never cause archive traffic to fall back to the control connection. Disabling management-network storage sync on either node denies relay. For direct-only operation, keep a shared dedicated storage-sync network and disable that permission.

Relay is only a fallback for TCP connection failure before transfer begins. TLS, token, and transfer-verification failures do not trigger a switch. Disconnected transfers must retry the whole task from the beginning; resume is not supported. Relay retains end-to-end encryption and rate limits. It permits one active task per node and two per server process, sharing an aggregate 64 MiB/s budget per process; lower operator or sync-group limits still apply. **Test transfer connection** for images identifies the direct or relay probe path; server sync logs record the actual model path. See [Peer Sync](peer-sync-models.md).

TCP internal tests below always check real direct connectivity between the selected addresses. Successful relay does not change a failed TCP test. Archive relay does not provide NCCL/Gloo or RDMA connectivity. Compute- and serving-network requirements also remain unchanged.

**Compute: check the network actually used by the entire group**

If all nodes in a multi-node deployment share one compute network, they use it. If there is no compute network shared by all nodes (none are attached, only some are attached, or nodes belong to different compute networks):

- If all nodes allow the management network to carry compute traffic, the entire group falls back to it, and evaluation reports a `COMPUTE_NETWORK_FALLBACK` warning.
- If any node disallows compute traffic on the management network, the deployment is rejected (`COMPUTE_NETWORK_NONE` / `COMPUTE_NETWORK_MIXED` / `COMPUTE_NETWORK_MISMATCH`). To prevent fallback, disable the management network's permission to carry compute traffic on these nodes.

Check the actual communication addresses and devices in the command preview and instance details. Each rank's `VLLM_HOST_IP` uses its own IP on the selected network, and rank 0's address is used as the master. For host networks, NCCL/Gloo is configured for the selected interface. **Compute network** in a multi-node instance's details shows the network, interface, and IP that rank actually used at startup, and indicates fallback when it uses the management network.

Behavior and restrictions by type:

- **Host Ethernet**: Sets `NCCL_IB_DISABLE=1` and uses TCP.
- **Host RDMA**: Sets `NCCL_NET=IB`, provides all HCAs to NCCL and the first HCA to UCX, and exposes `/dev/infiniband` and the required locked-memory permissions to the container. Startup fails if the container cannot see the verbs devices; it does not silently fall back to TCP. Environment variables of the same name set by a template or user still take precedence. runveil (runtime identifier `udocker`) cannot expose RDMA devices to containers and cannot use this type of compute network (`COMPUTE_NETWORK_RUNTIME`).
- **Accelerator RDMA**: Provides HCCL with the bootstrap IP and interface, and mounts the host's `/etc/hccn.conf` read-only. It uses accelerator ports and does not inject `/dev/infiniband`. Ascend multi-node deployments must share this type of network (`COMPUTE_NETWORK_ACCELERATOR_REQUIRED`), with at least one usable card on each node (`COMPUTE_NETWORK_NO_CARDS`).
- **Accelerator HCCS**: Selects Mooncake/HIXL's `hccs:device` path for A3/910C, and automatically sets the HCCS initialization and Fabric Memory parameters. It still mounts `/etc/hccn.conf` read-only for the Ascend runtime, while the KV path uses HCCS rather than RoCE.

PD disaggregation also checks the KV transfer network according to the mode:

- Cross-machine `vllm-ascend-mooncake` requires an Accelerator RDMA or Accelerator HCCS network (`PD_NETWORK_ACCELERATOR_REQUIRED`).
- Modes that use UCX or the Mooncake transfer engine (`vllm-nixl`, `sglang-nixl`, `vllm-mooncake`) cannot use an Accelerator RDMA network (`PD_NETWORK_HOST_REQUIRED`).
  `vllm-mooncake` uses Mooncake's `rdma` protocol on a Host RDMA network and `tcp` on other networks.
- PD deployments that support sleep cannot use a Host RDMA network for KV transfer (`PD_SLEEP_RDMA_UNSUPPORTED`); creation is rejected. If an RDMA network is attached after creation, the deployment remains unschedulable with the same reason. Ordinary multi-node deployments that use RDMA for collective communication are not subject to this restriction; see the limitations in [sleep / wake](sleep-wake.md).

For specific variables and container arguments, see [Deployment Launch Parameter Sources](../reference/deployment-launch-sources.md#拓扑).

**Serving: verify the forwarding port from the server to the node**

The serving network firewall must allow traffic from the server to `19443/tcp` on the node's serving IP. The agent provides TLS forwarding on this port. The gateway connects using a per-instance token and forwards to the instance's loopback port. Callers continue to use an API key through the gateway and do not access this forwarding port directly.

When a node has a serving network, it is **accessed only through that network, with no fallback to the agent tunnel**:

- After an instance is scheduled to the node and before its container starts, the server connects to this port and completes a TLS handshake. If it is unreachable, the instance fails with a reason.
- If the serving network becomes unreachable for a running instance, requests fail immediately. Check the address and status in **Serving network forwarder** on the node details page; hover over an unreachable status to see the reason.

The server does not bind a source address when dialing; it selects one according to its local routing table. If it has a serving network interface on the same subnet, it usually uses that interface.

Without a serving network, nodes that allow the management network to carry serving use the agent tunnel. If this permission is disabled and no serving network exists, the scheduler does not place instances on the node (`serving_unavailable`).

After networks, attachments, or node labels change, the server sends updated configuration to online nodes. Running instances are unaffected and use the new network only the next time they start.

## Node Proxies and Download Addresses

Node outbound proxies (`http_proxy` / `https_proxy` / `no_proxy`) and model/image download addresses (mirrors, internal proxies, and disabling origin fallback) are not configured here. They are separate from the storage-sync, compute, and serving networks described above. Configure them by node label under Settings → Image & model pulls, in **Proxy** and **Source rules**, respectively. Node details include a read-only **Pull proxy** card showing the proxy this node actually uses and its dockerd status. For rules, proxies, examples, and troubleshooting, see [Configure Image and Model Pulls](configure-pull-settings.md). For the dockerd proxy on Docker nodes, see the [dockerd section](configure-pull-settings.md#dockerd).

Every 10 seconds, the agent collects transmit and receive rates, errors, and congestion for each interface and RDMA port, as well as node traffic by role. For metric names, labels, and measurement definitions, see [Network Metrics](../reference/network-metrics.md).

- The network inventory and RDMA port tables on the node's **Networks** page show real-time rates for the last two minutes.
- The **Monitoring** tab on network details pages (including the management network) charts that network's traffic, errors, and congestion per node, with selectable periods from 15 minutes to 24 hours.

## Internal Tests

Storage-sync, compute, and management network details pages have an **Internal test** tab. First determine which path you want to verify, then prepare the corresponding tools and scope.

### Select the Test Method and Scope

| Goal | Test method | What it verifies |
|---|---|---|
| Verify connectivity, RTT, or TCP throughput between host addresses | TCP | Selected Ethernet addresses; bootstrap addresses on RDMA networks |
| Check the bandwidth of each host RDMA HCA | RDMA bandwidth (ib_write_bw) | The host's RDMA path; this is not equivalent to validating NCCL inside a container |
| Check reachability between Ascend cards with matching numbers | Card reachability · same-named cards | Only pairs of cards with matching numbers |
| Determine the actual wiring between Ascend cards with different numbers | Card reachability · full card matrix | All card pairs between the selected nodes |

The management network provides only TCP tests, covering all nodes that have reported management network addresses, even if dedicated networks have taken over all of its roles.

Choose which node pairs to test:

- **All pairs**: Covers every node pair. TCP and RDMA bandwidth tests use ordered node pairs: N nodes produce N×(N−1) pairs in N−1 rounds. Up to 128 nodes per test.
- **Ring**: Nodes are arranged in a circle by name. Each tests the next, and the last tests the first, for N pairs. TCP and RDMA bandwidth tests finish in one round, with each node sending and receiving exactly once. Ring is selected by default for more than 16 nodes. Up to 1024 nodes per test.

Ring tests can detect problems with individual nodes or HCAs, but cannot detect connectivity failures that occur only between nonadjacent nodes, such as a fabric split into two halves where adjacent nodes happen to be in the same half. See below for accelerator card pairing and test duration.

### Prepare TCP or Host RDMA Tests

**TCP** connects from the source node's address on the network to a temporary port on the target node's corresponding network address and measures authenticated-handshake RTT. Optionally, it measures throughput for a few additional seconds. Each node sends and receives only one stream per round. On RDMA networks, TCP uses only the bootstrap interface.

**RDMA bandwidth (ib_write_bw)** first checks the HCAs and tools:

- Each node pair starts a server on the target HCA, then connects from the source HCA. HCAs run in parallel, and bandwidth is the sum across HCAs. HCA IPs on the same subnet are paired first (including point-to-point /30 and /31 subnets); the rest are paired in order. Extra HCAs fail with “This HCA has no HCA to pair with on the other node”. The handshake uses the target node's bootstrap address.
- RoCE uses an IPv4 RoCE v2 GID, matching the default selection in NCCL 2.21 and later. If the RoCE interface has no IPv4 address, the test reports failure directly rather than letting the tool select a GID itself.
- The tool first uses `ib_write_bw` from the host's `PATH`, then the version bundled with the agent. The bundled version dynamically links to the host's `libibverbs` and `librdmacm` and can run once userspace libraries such as rdma-core or MLNX_OFED are installed. If neither version can run, the affected nodes are listed before the test starts, and the test also fails with “Cannot run ib_write_bw; install perftest”. The package manager installs the userspace libraries along with perftest.

TCP and RDMA bandwidth tests run on the agent host, covering the actual paths for storage sync and Host Ethernet compute networks. They do not validate the NCCL path of inference ranks inside containers. The NCCL version in a custom image or `NCCL_IB_GID_INDEX` may cause a container to select a different GID. If RoCE tests pass but container communication fails, check [NCCL GID and RoCE configuration](https://docs.nvidia.com/deeplearning/nccl/user-guide/docs/env.html#nccl-ib-gid-index) for the image's version. Do not assume that GID index 0 always represents a particular RoCE version.

### Prepare Accelerator RDMA Tests

- **TCP** verifies only the bootstrap addresses used for handshakes; it does not pass through accelerator ports.
- **Card reachability · same-named cards** (`card_ping`, default) has cards with matching numbers on two nodes ping each other, such as `npu3` to `npu3`. It is suitable for routine checks.
- **Card reachability · full card matrix** (`card_matrix`) has every card on one node ping every card on another node. Use it to verify wiring when setting up the network. You cannot determine reachability between different card numbers from addresses alone.

The agent runs `hccn_tool -i <card-index> -ping` for Accelerator RDMA or
`hccn_tool -i <card-index> -hccs_ping` for Accelerator HCCS to test
reachability, not bandwidth. A reply proves bidirectional reachability, so
each pair of nodes is tested only once: N×(N−1)÷2 pairs for All pairs and N
pairs for Ring.

Tests stagger nodes and cards across rounds, with each card sending only one ping at a time. An unreachable probe waits up to 3 seconds per round. Estimated duration is based on the number of rounds, not the sum of all individual probes. Use the following table to choose the scope:

| 24 nodes × 8 cards | Probes | Rounds | Maximum duration |
|---|---|---|---|
| Ring + same-named cards | 192 | 1 | 3 seconds |
| Ring + full card matrix | 1536 | 8 | 24 seconds |
| All pairs + same-named cards | 2208 | 12 | 36 seconds |
| All pairs + full card matrix | 17664 | 96 | 4.8 minutes |

The form shows the node count, probe count, round count, and estimated maximum duration. A combination cannot start if rounds × 3 seconds exceeds 10 minutes (`NETWORK_TEST_TOO_LONG`). Reduce the scope or switch to Ring or same-named cards.

### Start a Test

1. Select the test method, **Nodes** (all nodes on the network, selected nodes, or filter by label), and **Pairs** (All pairs or Ring). At least two nodes are required. To test TCP throughput, select **Measure throughput** and set the seconds per pair.
2. Check the address, HCA, and tool notices before starting: nodes without an address (bootstrap for RDMA) or a usable HCA are listed. For card tests, also check the estimated duration. Correct configuration that affects this test, then click **Start test**.
3. Wait for results. Review **Problems** first, then open failed node pairs for details. Use the result descriptions below to fix wiring, addresses, or tools. Run the same scope again to confirm the problems are resolved.

Each network retains its 10 most recent results. Tests in progress when the server restarts are marked as interrupted.

### View TCP and RDMA Bandwidth Results

**Problems** groups failures by node, HCA, and reason, and states how many pairs are affected. For an HCA that should not be used, click **Remove HCA** directly; run the test again to see the effect.

Below, All pairs shows a node matrix, and Ring shows a node-pair list. Each pair is classified as **Passed / Partial (some HCAs failed) / Failed**. Click or press Enter to view source and target addresses and RTT. RDMA tests also list the source and target of each HCA pair, their respective IPs, and results.

### View Accelerator Card Reachability Results

The full card matrix helps you decide which cards to select for deployment. When testing only same-named cards, “All same-named” only proves reachability between cards with matching numbers; it does not establish reachability for other combinations. Results represent only the moment of the test. Retest after wiring or links change.

| Shape | Meaning | Conclusion |
|---|---|---|
| All cards | Every card pair is reachable | Any combination of card numbers can be used |
| Same-named only | Only cards with matching numbers are reachable (direct wiring by card number, or separate planes by card number) | Select the same card numbers at both ends |
| Partly reachable | Any other case | Wiring is irregular; select cards based on each node pair's results |
| None | No card pair is reachable | Check accelerator port links and addresses |
| Not tested | A node is offline, has no cards, or has no matching card numbers to test | Check the reason for that node pair |

Node-pair status is determined by shape: All cards and Same-named only both support deployment and count as **Passed**; Partly reachable counts as **Partial**; None and Not tested count as **Failed**.

If most node pairs have the same shape, the results give the corresponding conclusion and identify pairs that differ to help locate wiring or configuration problems. In the node-pair overview, All pairs shows a node × node table, and Ring shows a list; each cell displays that pair's shape.

Click or press Enter to open a node pair: the full card matrix shows a source-card × target-card grid, while same-named cards shows a list of individual pairs. Hover to see both addresses, RTT, or the failure reason. When testing only one node pair, this grid is shown directly.
