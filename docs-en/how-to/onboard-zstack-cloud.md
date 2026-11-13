# Onboard ZStack Cloud

Add an existing ZStack Cloud environment to GPUWorks, synchronize its KVM hosts and GPU resources, and install the agent on the hosts you want to manage.

## Version and Environment Requirements

**Requires ZStack Cloud 5.4.2 or later.** The credentials form displays this notice when adding or editing ZStack Cloud.

This minimum is based on the current implementation's source dependencies: the AK request date format requires the parsing support introduced in Cloud 5.4.0, and the GPU `allocateStatus` field was introduced in Cloud 5.4.2. Adding an environment, resource synchronization, agent installation, and GPU reservation and release have not yet been tested end to end on 5.4.2; this minimum does not mean that version has passed full compatibility validation.

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

1. In the **Connected external platforms** list, find the environment (its External platform column shows ZStack Cloud and its Platform type column shows Private cloud) and click **Sync**. **Sync All** synchronizes every platform in the list that supports it, public cloud accounts included.
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
