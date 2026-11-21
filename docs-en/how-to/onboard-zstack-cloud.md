# Onboard ZStack Cloud

Add an existing ZStack Cloud environment to GPUWorks, synchronize its KVM hosts and GPU resources, and install the agent on the hosts you want to manage.

## Version and Environment Requirements

**Requires ZStack Cloud 4.8.0 or later.** The credentials form displays this notice when adding or editing ZStack Cloud.

GPUWorks uses ZStack Cloud 4.8.0 as the minimum version for its integration and adapts to different versions according to their API capabilities. This minimum requirement does not mean that every later version has been tested in a real environment.

GPUWorks handles physical GPUs according to the data Cloud provides:

| Protocol shape | Ownership determination |
|---|---|
| PCI device query only | PCI device status and VM ownership |
| GPU query without a separate allocation field | Reuses PCI status and VM ownership already included in the GPU response, without extra requests |
| GPU query with a separate allocation field | Cloud's reported GPU allocation state |

A missing allocation field does not mean that a GPU is free: PCI information must establish that the physical GPU is unoccupied before it can be used. An explicitly occupied, empty, or unknown allocation value is never made available through PCI interpretation. GPU disabling, enabling, and IOMMU group effects retain Cloud's behavior.

AK request dates use a format accepted by these protocols. When CPU reservations cannot be configured per host, GPUWorks reads the global value and leaves host CPU reservations unchanged. Adding an environment confirms credential validation; resource synchronization and GPU use still require the corresponding permissions and valid states.

These protocols are covered by source-based HTTP fixtures, including reservation, release, and sharing. Real-environment validation has passed on Cloud 4.8.0 Enterprise with one node and one GPU, and on Cloud 5.5.32 with one node and two GPUs: AK onboarding, synchronization, agent installation, inference, release on stop while preserving the original description, and inference after restarting. The 5.5.32 validation also covered regular and streaming inference, the browser Playground, and independent deployment and release on the two GPUs. Other versions, multiple nodes, vGPU, and GPU sharing still require their own real-environment validation.

Before onboarding, confirm that:

- The GPUWorks server is running, and you sign in with an account that can manage cloud accounts and nodes.
- The server can reach the Cloud API, and the Cloud license and version support AccessKey authentication.
- Each host selected for agent installation is a connected KVM host with a reachable management address and SSH connection information.
- For agent-initiated connections, the host can reach the GPUWorks server connection address. For server-initiated connections, the server can reach the agent's listening port on the host. See [Connect Nodes](connect-nodes.md) for address and port requirements.

## Prepare Connection Credentials

Prepare an administrator AccessKey in Cloud. Confirm that it is enabled and that its account can query hosts, GPUs, and resource configurations, perform coordinated GPU resource operations, and execute host onboarding operations.

| Field | Value |
|---|---|
| Name | A custom environment name to distinguish Cloud environments in GPUWorks |
| API Endpoint | The Cloud management node's API endpoint, such as `http://MANAGEMENT_NODE_IP:8080/zstack`; replace the placeholder with the actual address |
| AccessKey ID | The administrator's AccessKey ID |
| AccessKey Secret | The Secret corresponding to that ID |

The endpoint supports the `/zstack` context and an optional `/v1` suffix. Use a local Cloud AccessKey; no login session needs to be saved or refreshed. An existing Session Token cannot be converted to an AccessKey; edit the credentials to enter the ID and Secret.

GPUWorks and Cloud system clocks must be synchronized. When `rest.checkTimeZone=false`, Cloud ignores time zone offsets, so both systems must also use the same time zone. Queries and resource modification requests use AccessKey signing; the Secret is not sent with requests.

## Add a ZStack Cloud Environment

1. Open **External Platforms** under **Compute** in the sidebar (`/providers`).
2. Click **Add External Platform**, or select the **ZStack Cloud** card under Quick Connect.
3. Select ZStack Cloud in the **External platform** field, review the version notice, and enter the name, API Endpoint, AccessKey ID, and AccessKey Secret.
4. Click **Add**. GPUWorks calls the Cloud API to validate the credentials and saves the environment only if validation succeeds.

Successful credential validation confirms access to the API checked at that step. Resource synchronization and agent installation also require their corresponding permissions and network connectivity. If adding the environment fails, use the displayed reason to check the endpoint, key state, permissions, version, and system clocks before retrying.

## Synchronize Hosts and GPUs

1. In the **Connected external platforms** list, find the environment (its Vendor column shows ZStack Cloud and its Platform type column shows Private cloud) and click **Sync**. **Sync All** synchronizes every platform in the list that supports it, public cloud accounts included.
2. Expand the environment row and check the synchronized hosts, management addresses, and GPU models and counts. Instances in this list represent KVM hosts managed by Cloud.
3. If synchronization fails, inspect the account state and error message, fix API, permission, or version issues, and synchronize again.

The instance inventory reflects the most recent synchronization. Synchronize again to obtain updated Cloud resource information.

## Install the Agent and Confirm Onboarding

1. Click **Install Agent** for the host you want to manage.
2. Select the connection mode and check the server or compute node connection address. If SSH credentials are needed, enter a valid password or private key under Advanced Settings.
3. Click **Install** and review the precheck and installation report. On failure, retain the reported reason, fix the issue, and retry.
4. After installation, open **Compute Nodes**, confirm that the host is online, and check its GPU and runtime information.

See [Connect Nodes and Upgrade or Uninstall the Agent](connect-nodes.md) for installation directories, startup methods, and subsequent agent upgrades and removal.

## Update Connection Credentials

Click **Edit** for the environment in the **Connected external platforms** list to update the name, endpoint, or AccessKey. Credential fields left blank retain their existing values; the **External platform** field cannot be changed. After saving, synchronize again to confirm that the new credentials can query resources.

Credentials are encrypted at rest, and the Secret is not displayed in the edit form.

<!-- translation-source-sha256: 59a2f61ce6b09e6d359dc5d38e1927d6689dfd3ef51dd74ac14b7a35beb573c1 # Onboard ZStack Cloud -->
<!-- translation-source-sha256: 04ef952749c1c2d26bd7a96c3fb27c10d09359f0af553d1cfc6d1132b1bab7f2 ## Version and Environment Requirements -->
<!-- translation-source-sha256: ba20fd9b85931800020da31047aa04d7aaa50456fe006779fca6976cf39be5f8 ## Prepare Connection Credentials -->
<!-- translation-source-sha256: 470a788fc390e6afd275706bfa69177997b0203b2cb9642ffda014f427fae2ef ## Add a ZStack Cloud Environment -->
<!-- translation-source-sha256: 1480b86c84beb0b2792a97dd3eb991d4b7071b17406cffee887fa17f2436248e ## Synchronize Hosts and GPUs -->
<!-- translation-source-sha256: 01946b76a8705285cb11311107c1628fd97a81ffdf62ccc48e7eb6d6592117d6 ## Install the Agent and Confirm Onboarding -->
<!-- translation-source-sha256: dec10c3f5c587f6ad9f439698809f208fbccd1bba5b87ab0907b11ba7e897f2d ## Update Connection Credentials -->
