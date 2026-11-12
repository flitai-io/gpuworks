# ADR 0063: After User Confirmation, the Server Applies the Connect Manifest to a Managed Cluster with a Short-Lived kubeconfig Issued on the Spot by the Cloud Provider

Date: 2026-10-22. Status: Accepted (amends item 2 of [ADR 0052](0052-kubernetes-nodes.md), “the server holds no kubeconfig and does not connect to the apiserver”).

## Background

When connecting managed Kubernetes (Alibaba Cloud ACK, DigitalOcean DOKS), users must find the GPU node labels and run `kubectl apply` themselves. Yet the cloud account is already in gpuworks, and the cluster list, node pools, and cluster credentials can all be obtained from the cloud API. ADR 0052 decided that the server does not touch the cluster so as not to keep a long-lived, highly privileged kubeconfig.

## Decisions

1. **Clusters are listed live from the cloud account and are not stored.** A provider driver can implement `ClusterProvider` (list clusters and GPU node pools, issue a kubeconfig). When the user picks a GPU node pool to connect, an ordinary cluster entry is created; the entry records only the additional source (cloud account, cluster ID), and the node selector is generated from the node pool.
2. **The server connects to the cluster only within the single operation the user confirms.** Both “Apply to the cluster” and “Remove from the cluster” first run a server-side dry run that lists each object that would be created, updated, or deleted, and execute only after the user confirms. Each operation fetches a kubeconfig from the cloud API on the spot (valid for 15 minutes for both ACK and DOKS) and discards it after use; it is **neither stored in the database nor written to logs**. Only the certificate or token carried in the kubeconfig is honored; a kubeconfig that needs to run a program (exec, auth-provider) is rejected.
3. **Only gpuworks's own manifest is applied.** The server server-side applies (`fieldManager=gpuworks`) the fixed set of object kinds rendered by `kubemanifest.Render`, and rejects any other kind. Removal deletes the DaemonSet first (the agents, placeholder Pods, and instance Pods are deleted with it), and cluster-scoped objects (PriorityClass) still used by other gpuworks namespaces in the same cluster are kept (superseded by the 2026-11-12 amendment: PriorityClass is named for the namespace, and removal deletes the entry's own).
4. **The manual path is unchanged.** Every entry still has a `kubectl apply` command; a cluster whose API Server has no public address can be connected only this way. An entry created by hand has no source, and the server never connects to it.
5. **Nodes of a managed cluster belong to the cluster.** The cloud host list marks the cluster each belongs to, and installing an agent, powering, releasing and resizing are all refused (`PROVIDER_INSTANCE_IN_CLUSTER`), to avoid contending with the DaemonSet and the node pool for the same machine. The server gives each list entry its `actions`, empty for a cluster node. The decision rests on the cluster recorded by the last sync, so a node that just joined must be synced once first.

## Amendment (2026-11-10): One Entry per Cloud Cluster, Holding Several GPU Node Pools of One Vendor

Item 1's “the user picks one GPU node pool to connect, and the node selector is generated from the node pool” becomes:

1. **An entry holds a set of the cluster's node pools.** The selected pools are rows of the child table `kube_cluster_pools` (the entry, the cloud's node pool ID, and the key of the node label the cloud puts the pool ID on), not a JSON column. The DaemonSet selects nodes with a required `nodeAffinity` (`In`, the values being those pool IDs); the driver supplies the label key: `alibabacloud.com/nodepool-id` on ACK and `doks.digitalocean.com/node-pool-id` on DOKS. The label key is stored with the pool, so the manifest still renders after the cloud account is deleted.
2. **A cluster still has only one entry** (the decision is unchanged). To use more of its node pools, edit that entry's node pools (`PUT /kube/clusters/{uuid}/pools`) and apply the manifest again for the change to take effect in the cluster; agents on pools taken away exit as the DaemonSet shrinks, and their nodes stay on the nodes page and are cleaned up as offline Kubernetes nodes usually are.
3. ~~**The node pools of one entry must be of one vendor** (the vendor decides which host paths the agent mounts); mixing them returns `KUBE_CLUSTER_MIXED_VENDORS`.~~ Superseded by the 2026-11-12 amendment: an entry has no vendor, and its node pools may be of any mix of vendors.
4. **A manual entry is unchanged**: it uses the free-form node selector and has no node pool rows; a cloud entry no longer accepts a node selector.
5. **Migration of old entries** (migration 204): an entry whose source is a cloud account's cluster and whose node selector is exactly one `key=value` pair has that pair turned into one node pool row and the selector cleared; an entry with a selector of any other shape is left as it is and keeps selecting by that selector until its node pools are edited (afterwards it selects by the pools alone). Because the selection became a `nodeAffinity`, the DaemonSet's Pod template changes the next time such an entry is applied again, and the agents restart once in a rolling update.

## Amendment (2026-11-12): An Entry Has No Vendor, Node Pools May Be Mixed, and Apply Replaces a Changed PriorityClass

1. **Node pools are no longer limited to one vendor.** The 2026-11-12 amendment to [ADR 0052](0052-kubernetes-nodes.md) removed the entry's vendor (each agent detects its node's accelerator itself, and unsupported nodes stand by), so the premise that "the vendor decides which host paths the agent mounts" no longer holds; creation and `PUT …/pools` therefore no longer check the vendor, and the `KUBE_CLUSTER_MIXED_VENDORS` error is gone. Node pools of different vendors in one cloud cluster can go into the same entry.
2. **Apply replaces a changed PriorityClass.** A PriorityClass's value and `preemptionPolicy` cannot be changed in Kubernetes, and a server-side apply is rejected. After an entry's priority or allow preemption is edited, “Apply to the cluster” has the server delete and then create the object, the action of that item in the apply result is `replace`, and the dry run lists it likewise.
3. **Removal deletes the entry's own PriorityClass.** A PriorityClass is named for the namespace (`gpuworks-inference-<namespace>`), so other entries of the same cluster do not share it; removal deletes it together with the DaemonSet, the RBAC objects, and the namespace, and the earlier rule that cluster-scoped objects still used by other namespaces are kept is no longer needed.
4. **Removal has manual commands that do not depend on the token.** The entry DTO's `remove_commands` gives the kubectl commands (delete the DaemonSet, ClusterRoleBinding, ClusterRole, PriorityClass, and namespace) for entries the server cannot reach; for a cloud entry the server performs the same deletion.

## Trade-offs

- Not storing the kubeconfig costs one extra cloud API call per operation, and the cloud account must have permission to issue cluster credentials (ACK also requires the sub-account to have permissions in the cluster's RBAC).
- There is no “automatic sync” or background reconciliation: cluster changes (node pool scaling) are followed by the DaemonSet itself, so the server does not need a standing connection.
- client-go is not introduced: the manifest has only nine kinds of objects, a minimal REST client is enough, and it matches the agent side (item 4 of ADR 0052).
