# Connect a Kubernetes Cluster

**Use case**: You already have a Kubernetes cluster with GPUs and want to hand some of its GPU nodes to gpuworks for model deployment without installing an agent on each machine.

For step-by-step instructions for managed public cloud clusters (Alibaba Cloud ACK, DigitalOcean DOKS, Nebius mk8s), see [Connect Managed Kubernetes from a Public Cloud](connect-managed-kubernetes.md).

**Prerequisites**:

- The server is running, and the cluster nodes can reach the server address (the agent connects outbound; no inbound node port is needed).
- The cluster uses containerd (kubeadm, k3s, RKE2, microk8s, and GPU Operator environments all do). CRI-O (OpenShift) and Docker nodes are not supported; the agent refuses to start and states the reason.
- One cluster entry corresponds to nodes of one vendor: NVIDIA, Ascend, Hygon, or PPU. On NVIDIA nodes, the NVIDIA container runtime must be available: either as containerd's default runtime (as on managed Kubernetes such as DOKS, Nebius, and ACK), or through a RuntimeClass named `nvidia` (k3s creates it automatically). Other vendors only require the vendor driver on the node; they need neither the vendor container runtime nor the vendor device plugin.
- Nodes that mount NFS storage have `mount.nfs` installed (nfs-utils / nfs-common): the agent uses it in the node's mount namespace.
- You can run `kubectl apply` in the cluster, and a `privileged` namespace is allowed (the manifest labels the namespace with the Pod Security Admission `privileged` label; policies such as Kyverno or Gatekeeper must be relaxed separately).

## How It Works

A cluster joins as one DaemonSet. One agent runs on each selected node and exchanges the cluster's connect token for its own node token; from then on it is exactly like an ordinary node: scheduling, deployment, gateway, logs, and terminals make no distinction. An instance becomes a Pod pinned to that node (`restartPolicy: Never`, host network, model directories mounted by hostPath at the same path), and `kubectl get pods -n gpuworks-system` shows every instance. Retries and rebuilds remain gpuworks's responsibility; Kubernetes does not restart failed instances.

Each card has only one allocator at a time. When a device plugin (`nvidia.com/gpu` and similar) is installed on the node, the cards belong to the kubelet: for each card gpuworks wants to use, it starts a placeholder Pod (`gpuworks-hold-*`) that requests only one card. After all instances have stopped and the cluster's “Idle return” time has elapsed, the placeholder Pods are deleted and the cards go back to Kubernetes. Cards held by other Pods appear in gpuworks as externally occupied and are not scheduled. When the node has no device plugin, gpuworks uses the node's cards directly. When the cluster uses HAMi, ACK GPU sharing, or another way of splitting a card into several shares, borrowing works differently; see the next section.

## When the Cluster Uses HAMi, ACK GPU Sharing, or Other Splits

How cards are borrowed is chosen automatically from what runs on the node; the cluster entry needs no configuration:

| On the node | What gpuworks does |
|---|---|
| A plain device plugin (`nvidia.com/gpu`, one unit per card) | As above: the placeholder Pod requests 1 card |
| HAMi (the node has a `hami.io/node-*-register` annotation) | The placeholder Pod asks HAMi's scheduler for one whole card and names it: the card wanted is the card borrowed |
| ACK GPU sharing (the node has the `aliyun.com/gpu-mem` resource, or the `ack.node.gpu.schedule` label is `share`, `cgpu`, or `core_mem`) | The placeholder Pod requests the memory of one whole card, and the card it got is read from the allocation annotation the scheduler writes on the Pod |
| Other splits: MIG, time-slicing / MPS, Ascend vNPU templates without HAMi's registration | No cards are borrowed: all of the node's cards appear as externally occupied, the reason (MIG, time-slicing, vNPU templates, ...) is stated in the card's occupancy information, and no instance is placed on them; they recover automatically once the cluster returns to whole-card allocation |

Once a whole card is borrowed in any of the three modes, dGPU slicing on it (NVIDIA HAMi-core, Ascend vCANN, PPU HGGC) is exactly as on an ordinary node: instance Pods request no HAMi or ACK resource, and gpuworks' own dGPU slices a card among several instances.

**HAMi**: the placeholder Pod is not bound to the node. It sets `schedulerName: hami-scheduler` and `nodeSelector: kubernetes.io/hostname=<node>`, requests `nvidia.com/gpu: 1`, `nvidia.com/gpucores: 100`, and `nvidia.com/gpumem-percentage: 100`, and names the card's UUID in the annotation `nvidia.com/use-gpuuuid`. HAMi's device plugin only serves Pods that went through HAMi's scheduler and carry its allocation annotation, so borrowing cannot bypass the scheduler. Who owns the card is whatever HAMi wrote in `hami.io/vgpu-devices-allocated` on the placeholder Pod, checked to be the card asked for, with 100% of the compute and as much memory as HAMi registered for the whole card; otherwise the Pod is returned and the reason reported. When other Pods hold part of a card, they appear in gpuworks as externally occupied (Pod name, memory, compute); when the placeholder Pod stays Pending because of that, the borrow fails and the error carries the scheduler's reason (for example `CardInsufficientMemory`) and the Pods holding the card.

- HAMi's scheduler is named `hami-scheduler` by default. If the cluster renamed it, set an environment variable on the agent DaemonSet: `kubectl -n gpuworks-system set env ds/gpuworks-agent GPUWORKS_KUBE_HAMI_SCHEDULER=<scheduler name>`. With a wrong name no scheduler handles the placeholder Pod and the borrow times out; the error mentions this variable.
- HAMi on Ascend follows the shape in HAMi's documentation (request `huawei.com/<model>: 1` without memory, and the annotation `hami.io/use-<model>-uuid`); it has only been verified against test fixtures, not on hardware.
- Installing HAMi on Alibaba Cloud ACK takes two more steps: (1) the kube-scheduler the chart generates uses the default leader lease `kube-system/kube-scheduler`, which ACK's managed scheduler holds, so HAMi's scheduler never becomes leader and its extender keeps reporting `Scheduler extender has not become leader yet`; with a single replica set `scheduler.leaderElect=false`; (2) the Kubernetes 1.36 kube-scheduler lists and watches `resourceclaims`, `resourceslices`, and `deviceclasses` in `resource.k8s.io`, and the chart's ClusterRole lacks these read-only permissions, so they must be added.

**ACK GPU sharing**: supported only on ACK Pro clusters. The placeholder Pod uses the default scheduler, selects this node with `nodeSelector`, and requests `aliyun.com/gpu-mem: <GiB of one card>`. One card's memory is the node's `aliyun.com/gpu-mem` divided by `aliyun.com/gpu-count`, cross-checked against the label `aliyun.accelerator/nvidia_mem`; if the cards differ in size or the count does not match the probe, no cards are borrowed and the reason is stated. This mode has no way to name a card, so borrowing works as with a plain device plugin: one placeholder Pod per free card, the card each got read from the card number in `scheduler.framework.gpushare.allocation`, the wanted ones kept and the rest returned. Other Pods' copy of this annotation records the cards and GiB they hold, and they are shown as externally occupied.

**Extra permission the agent needs**: HAMi and ACK GPU sharing record who holds how much in annotations on the holding Pods, so the agent reads the Pods on its own node in every namespace (read-only: `get` and `list` on `pods` in the manifest's ClusterRole). Clusters connected with an older manifest get this permission only after `kubectl apply` again; without it the agent only sees which Pods the kubelet gave cards to, not how much each holds, and borrowing and ownership checks are unaffected.

**The node changes mode midway**: if the cluster changes the node from `nvidia.com/gpu` to ACK GPU sharing (or installs HAMi) while the platform holds cards, the new scheduler cannot see the old placeholder Pods and may give the same card to another Pod. When gpuworks notices, those cards appear in gpuworks as externally occupied with the reason “allocated by A when the platform borrowed this one and by B now; Kubernetes may give the card to another pod”, no more cards are borrowed in the old mode, and no new instance is placed on them; instances already on the cards are not affected, and once no instance runs on a card its old placeholder Pod is deleted. When the node goes back to its earlier mode, the cards recover automatically.

The device nodes an instance needs (Ascend `/dev/davinci*`, Hygon `/dev/kfd` and `/dev/dri/*`, PPU `/dev/alixpu*`, and so on) are handed to the instance by the agent's own device plugin: one extended resource per card, `gpuworks.ai/accelerator-<card number>`, and `gpuworks.ai/accelerator-shared` for the control nodes shared by all cards. The instance Pod requests the cards it uses and the shared resource, and the kubelet gives the container only those device nodes, so the instance needs no privileged mode. Which device nodes belong to which card comes from the vendor's own device injection, consistent with docker / nerdctl instances on ordinary nodes. Instances that need devices beyond the plugin (for example InfiniBand `/dev/infiniband`) run privileged.

The agent uses the node's own DNS (`dnsPolicy: Default`), like an agent installed directly on the host, and does not depend on cluster DNS; the server address must therefore be one the node can resolve, not an in-cluster Service name. The agent runs nerdctl (pulling and importing images, terminals) and `mount` (NFS) in the node's mount namespace, so the containerd socket and registry configuration (the `certs.d` generated from k3s's `registries.yaml`) are used at the node's own locations and need no configuration. When Docker's bundled containerd also runs on the node, the agent identifies the one the kubelet uses from its own container's shim.

## Connect

1. On the “Kubernetes” page, click “Connect cluster”, fill in the name, the server address (the address cluster nodes use to reach the server), and the vendor, and optionally the node selector (for example `nvidia.com/gpu.present=true`; empty means all nodes). When one Kubernetes cluster has nodes from two vendors, create two entries, one per vendor, each with a different namespace and node selector.
2. Options under “Advanced”:
   - State directory: the agent's directory on the node, `/opt/gpuworks` by default. When a gpuworks agent is already installed on the node, use a different directory (for example `/opt/gpuworks-kube`) so the two do not interfere.
   - containerd socket: when empty, the agent finds it itself (the default locations of containerd, k3s, RKE2, and microk8s); fill it in if it is installed elsewhere.
   - API server address: only for clusters where nothing on the node provides Service addresses (no kube-proxy or replacement, such as KubeEdge or OpenYurt edge nodes). Write it as `https://host:port`; the address must be in the API server's certificate. When empty, the agent reaches the API server through the cluster's `kubernetes` Service.
   - Agent image registry: where the nodes pull the agent image from: Docker Hub (the default), Huawei Cloud SWR or quay.io. Clusters in mainland China usually cannot reach Docker Hub; choose Huawei Cloud SWR. The image's tag is the version of the agent's runtime (such as `r1`) and does not follow the server's version; see "Upgrade the agent" below.
   - Agent image: when filled in, your own image runs instead of one from the registry above. For nodes without internet access, push `gpuworks/gpuworks-agent:r1` to a private registry the cluster reaches (or import it on each node with `ctr -n k8s.io images import`) and enter that address; nothing needs to be pushed again until the runtime version changes. For the image archive in offline environments, see [Deploy in an Offline Environment](deploy-offline.md#agent-image-for-kubernetes-clusters).
   - Image pull secret: when the agent image is in a registry that needs a login (an enterprise Harbor or a cloud provider's private registry), first create a docker-registry Secret in the namespace (`kubectl -n gpuworks-system create secret docker-registry regcred …`; create the namespace first if it does not exist), then enter its name here. The agent Pod uses it to pull the image, and the placeholder Pods the agent starts reuse it.
   - RuntimeClass: NVIDIA only: the RuntimeClass instance Pods use to reach the GPUs. When empty (the default), the agent on each node decides: it uses the cluster's `nvidia` RuntimeClass if there is one, otherwise the node's default runtime; when `nvidia-smi` is not present in the agent's own container, it runs it on the node. Fill it in only when the RuntimeClass has a different name.
   - Engine port range: `40000-42767` by default. Do not overlap the cluster's NodePort range (`30000-32767` by default): kube-proxy diverts NodePort traffic sent to local addresses.
   - Idle return: how long a borrowed card is kept after the last instance stops, 60 seconds by default (checked every 30 seconds, so the actual return happens between 60 and 90 seconds); “Keep borrowed cards” means never return them, handing the whole node pool to gpuworks.
   - Priority: the value of the PriorityClass (`gpuworks-inference-<namespace>`) of gpuworks hold and instance Pods, 10000 by default, from 0 to 1000000000; larger runs first, and ordinary Pods are 0. It also sets the eviction order when a node runs short of memory or disk: lower priority is evicted first.
   - Allow preemption: off by default (the PriorityClass says `preemptionPolicy: Never`): a hold Pod that does not fit only waits, and the scheduler never evicts your other Pods for it. On, with HAMi or ACK GPU sharing (where hold Pods go through the scheduler) the scheduler may evict lower-priority Pods; with a plain device plugin the hold Pods, like all instance Pods, are bound to the node without the scheduler and the switch makes no difference.
3. Copy the generated command and run it in the cluster:

   ```bash
   kubectl apply -f 'https://server.example/kube/manifest.yaml?token=…'
   ```

   The selected nodes appear on the Nodes page automatically, carrying the system labels `gpuworks.ai/kubernetes-cluster` (the value is the cluster UUID, shown as the cluster name) and `gpuworks.ai/kubernetes-node` (the node name in Kubernetes); filter the Nodes page by them, or use them in a deployment's node requirements to keep it on one cluster. Nodes added later by scale-out join automatically as well. If no node has joined 5 minutes after creation, “Troubleshoot” in the list gives the `kubectl` commands to run and common causes (the agent image cannot be pulled, the RuntimeClass does not exist, the node selector selects no node, the agent cannot reach the server).

The command contains the cluster's connect token and is shown only to administrators. After the connect token is revoked, new nodes can no longer join, and nodes already joined are unaffected.

## Day-to-Day Operations

| Scenario | What to do |
|---|---|
| Maintain a node | Run `kubectl cordon` in Kubernetes: at the next status report (within a minute) gpuworks likewise stops placing new replicas on that node, and `uncordon` restores it in the same way; the node's scheduling status shows as stopped by the platform, with the reason “Kubernetes stopped scheduling onto this node” |
| Upgrade the agent | As for any node: select nodes on the Nodes page and click “Upgrade selected Agents”, or click “Upgrade Agent” in the node details. The agent runs from `agent/gpuworks-agent` in the node's state directory, and an upgrade replaces that program in place, without changing the DaemonSet or reaching a registry; instance Pods are unaffected, and the upgraded program outlives the Pod. The runtime version (the image tag) rarely changes; when a release raises it, its release notes say so, and applying the manifest once more (`kubectl apply` or “Apply to the cluster”) is enough. The image tag `kubectl` shows is the runtime version, not the agent's; the Nodes page shows the agent version |
| A node is deleted or scaled in | After a node has been offline for 15 minutes, gpuworks asks another online node of the same cluster to confirm; when the node no longer exists in Kubernetes and has no instances, it is removed automatically |
| Delete a node from gpuworks | Only the record is deleted and the agent is not uninstalled; if the DaemonSet still selects the node, it rejoins, so adjust the node selector first |
| Uninstall | Run `kubectl delete -f` on the same manifest address: the DaemonSet, instance Pods, and placeholder Pods are all deleted, and the cards go back to Kubernetes |

## Limitations

- Only containerd is supported: images are pulled and imported by the agent through the node's containerd (the kubelet's `k8s.io` namespace), not through the kubelet, so instance images do not use the Pod's `imagePullSecrets`; credentials for private registries are configured in gpuworks's image sources (the cluster entry's image pull secret covers only the agent image).
- Instances use host network and create no Service / Ingress; they are accessed through the gpuworks gateway.
- Vendor command-line tools (`npu-smi`, `hy-smi`, `ppu-smi`) run in the node's mount namespace and use the node's own C libraries and driver libraries, so the agent image is independent of the node's distribution.
- The agent runs privileged with hostNetwork and hostPID (required for probing and for running nerdctl and mount through the node's mount namespace).
