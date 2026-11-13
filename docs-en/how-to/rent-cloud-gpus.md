# Rent Cloud GPUs

Connect GPU instances from Alibaba Cloud, RunPod, CompShare, AutoDL, or Vast.ai to GPUWorks, then stop or release the cloud resources when you finish using them. Rent an instance in the provider's console first, then sync it and install the agent in this platform. This guide also lists the supported REST APIs for creating instances.

**Prerequisites**:

- The server is running (see [Install and Upgrade the Control Plane](install-server.md)). Sign in to the console with an account that has cloud account and node management permissions. The server can access the selected provider's API.
- You have an account and API credentials for the provider (see the credential fields in the table below), sufficient account balance, and a GPU instance at the provider (or plan to create one through the API below).
- The instance must be running during installation. For an agent-initiated connection, the instance must be able to access `server_url`; for a server-initiated connection, the server must be able to access the instance's agent TCP port. For SSH or Cloud Assistant installation requirements, see [Install the Agent to Add the Instance to Compute Nodes](#安装-agent让实例出现在计算节点).
- For an agent-initiated connection with the server behind NAT, provide a stable domain name or reverse proxy address reachable from the instance. Do not use a quick tunnel address (see [Public Access](expose-public-address.md)).

GPUWORKS handles cloud instances as follows: save cloud account credentials (encrypted at rest) → sync the instance inventory → install the
agent over SSH (prefer Cloud Assistant for Alibaba Cloud) → bring the instance online as a compute node. **The console's External Platforms page does not provide an interface for selecting an instance type, region, or placing an order**:
first rent an instance (select its type, region, and image) in the cloud provider's console, then return to GPUWORKS to sync and connect it. For REST APIs
to create or delete instances, see [Create and Release Instances Through the API](#用-api-创建和释放实例).

## Add a Cloud Account

1. Open **External Platforms** under **Compute** in the left menu (path: `/providers`).
2. Click **Add External Platform**, or click a provider card under **Quick Connect** in the center of the page to preselect its type. Links to the providers' official websites appear below the cards.
3. Select the vendor in the **External platform** field, enter a **Name** (required; choose your own) and the fields under **Credentials**, then click **Add**.

Credential fields for each type (the UI is authoritative; optional fields can be left blank):

| Type | Required | Optional |
|---|---|---|
| Alibaba Cloud | AccessKey ID, AccessKey Secret | RAM role ARN, regions (see [Alibaba Cloud](#阿里云)) |
| RunPod | API Key | API endpoint |
| CompShare | Public key, private key (CompShare API key pair) | API endpoint |
| AutoDL | API Key | API endpoint, SSH password (used as the default SSH password for subsequent agent installation) |
| Vast.ai | API Key | API endpoint |
| DigitalOcean | API Key (Personal Access Token) | API endpoint (see [DigitalOcean](#digitalocean)) |

The current AutoDL API syncs only **Pro container instances**, and one-click installation offers only **Agent-initiated connection**. Do not interpret this product limitation as the port capabilities of all AutoDL instances. For manual port configuration, follow the [provider's port documentation](https://www.autodl.com/docs/port/) and the instance's actual configuration.

RunPod supports only Secure Cloud pods, not Community Cloud pods (the add-account form also displays this notice).

Leave **API endpoint** blank to use the provider's default address. Change it only when using a proxy or private gateway.

When you save, the server calls the cloud provider's API once to validate these credentials. It saves them only if validation succeeds and shows the status as **Valid**. Otherwise, it displays
**Add failed: …** with the reason returned by the provider. Saved credentials are encrypted at rest and are no longer displayed in the UI. To update them later, click **Edit**
in the list: **fields left blank retain their existing values**. You cannot change the type.

## Sync Instances and Check Types, Regions, and Prices

1. Click **Sync** for an account in the **Connected external platforms** list (or **Sync All** at the top of the page to sync all external platforms at once). The list's **External platform** column shows the provider (such as Alibaba Cloud or RunPod), and its **Platform type** column shows **Public cloud**. On completion, the message reads **Synchronization complete; N instances found**.
2. Click the expand arrow on the left of the account row to view its instance table:

   | Column | Meaning |
   |---|---|
   | Instance name / Instance ID | Name and ID at the cloud provider |
   | GPU model / GPUs | Specifications returned by the provider; **Not returned** if unavailable |
   | Connection | Endpoints offered by the provider: **Direct** or **External platform proxy** (SSH, with a copyable `ssh` command; endpoints marked **Terminal only** support connections but cannot install the agent). Alibaba Cloud also offers **Cloud Assistant** (no SSH needed) |
   | Status | Running / Stopped / Unknown |
   | Price | Hourly price (for example, `¥1.98/hr`); if the provider returns no price, this may display `¥0.00/hr`, which does not mean it is free |
   | Region, Type | Region; type is **Container** or **Virtual machine** |

3. Account status changes with sync results. A failed sync marks the account as **Error** and displays **Some external platforms have errors** at the top of the page. Follow the prompt to check credentials and provider API availability, then sync again.

The list automatically refreshes account status every 15 seconds, but the instance inventory comes from the last sync. Click **Sync** again for the latest cloud state. Container instances usually have no usable Docker daemon; the agent uses the built-in runveil when Docker is unavailable. Being root inside a container does not mean you can run the host's Docker.

## Install the Agent to Add the Instance to Compute Nodes

First confirm that the instance is running, then prepare addresses according to the connection direction:

| Connection Direction | Addresses and Requirements |
|---|---|
| **Agent-initiated connection** (default) | **Management node address** is prefilled from the browser address. Change it to a stable address reachable from the instance; the instance does not need an inbound agent port |
| **Server-initiated connection** | Enter the **Agent listen address** (normally defaults to `0.0.0.0:18800`; CompShare defaults to `:3389`. For instances with port mappings, leave it blank for automatic selection as described below) and **Compute node address**. For the latter, use an IP address, domain name, or mapped `host:port` reachable from the server. If you enter only a host, the agent listen port is used. Security groups and firewalls must allow the corresponding TCP port. Do not enter `server_url` for this direction |

`server_url` must be an absolute `http://` or `https://` URL without user information, a query, or a fragment. A quick tunnel address displays a temporary-address warning and cannot serve as a long-term node address.

1. Find an instance that is not yet onboarded in the instance table, then click **Install Agent**. Connected instances show **Onboarded**.
2. In the page, select the **Connection** and **Connection direction**, and check the addresses against the table above. **Automatic** tries installation channels according to provider capabilities: Cloud Assistant is preferred for agent-initiated connections on Alibaba Cloud; otherwise, direct SSH is preferred, with other available endpoints tried after a failure. You can also select an endpoint explicitly; endpoints marked **Terminal only** cannot install the agent.
3. Under **Advanced Settings**, check the installation directory and **Start via systemd**, then review the credentials, ports, and restart conditions below and click **Install**. You do not need to create a compute node or obtain a token first.
4. Wait for the installation result and expand any failures or warnings. When **Agent installed and connected** appears, click **Done**. For **Agent installed, with notes**, inspect the report first. If **Agent started, not connected yet** appears, the agent is installed: fix connectivity first instead of reinstalling.
5. Open **Compute Nodes**, confirm that the node for this instance is online, and check its accelerator model, count, and runtime. If installation fails, retain the reason from the report, fix it, and click **Try again** in the page. Pending nodes whose installation has not succeeded are cleaned up.

An empty installation directory defaults to `/opt/gpuworks`. A custom directory must start with `/` and cannot be the root directory; `~` is not expanded. Installation fails if the specified directory is not writable, without falling back to `/tmp/gpuworks`.

**Start via systemd** is enabled by default: on a Linux instance with systemd and root execution available, it registers `gpuworks-agent.service`. When disabled, it uses nohup without starting at boot, and preserves existing host services and agents in other directories. Container instances usually lack systemd, so both settings use nohup. See [Startup Methods](connect-nodes.md#启动方式与后续重启).

### Credentials and Possible Restart Confirmation

SSH installation uses the platform key by default, so you usually do not need to enter a password. Preparation differs by provider:

| Provider or Scenario | What to Check Before Installation |
|---|---|
| Alibaba Cloud Cloud Assistant | No SSH is needed; the instance must be able to download the server's installation script and agent. Automatic selection switches to SSH for server-initiated connections. See [Alibaba Cloud](#阿里云) |
| RunPod | The platform public key is appended to the account's SSH keys, preserving existing keys. A pod writes account public keys only at startup, so a running pod does not automatically receive a new key. If a running pod lacks that public key and existing credentials cannot log in, installation requests restart confirmation. Alternatively, provide a working private key once under **Advanced Settings** to avoid adding a public key by changing the pod configuration |
| Vast.ai | The platform authorizes the public key on the instance through the provider API |
| CompShare, AutoDL | The platform tries the instance's root password returned by the API to log in and write the platform public key; the SSH password saved for the AutoDL account is a fallback |
| Automatic authentication fails | Provide a working password or private key for the instance under **Advanced Settings**, or authorize the platform public key as instructed by the error, then retry |

Once the platform public key is written successfully, subsequent recovery uses the platform key. If writing fails, the credentials that worked for this attempt may be retained. See report items such as `SSH authentication` and `Platform key` for the actual result. If precheck indicates that the public key is added only during installation, other checks requiring a login continue during installation.

**RunPod public key or port changes may require restart confirmation.** When `INSTANCE_RESTART_REQUIRED` appears, the pod has not yet been modified. Only after confirmation does the platform update the configuration, restart, and continue installation. **Restarting clears nonpersistent volumes, and port mappings (including SSH) may be reassigned**. Save data you need to keep first. If you cannot accept a restart, cancel and use working login credentials or another connection method. Use `Platform key (pod)` and `Server-dial port` in the report to check the results.

### Provider Differences for Server-Initiated Connections

- **Alibaba Cloud**: uploads the agent over SSH; the instance does not need to download files from the server. Precheck reads security groups and reports failure if inbound allow rules are missing. If only some sources are allowed, it prompts you to check the server's outbound address. It does not automatically modify security groups. If the agent is running but the server cannot connect yet, allow access and wait for automatic connection; no reinstallation is needed.
- **CompShare**: container instances (pods) are reachable only through TCP ports forwarded by the provider, using a different port number on `…podtcp.compshare.cn`. SSH is forwarded this way too, with port 23 inside the container. When both address fields are blank, the driver first selects an already open TCP port with no listening process, preferring the **Agent listen address** port (3389 by default). If none is available, it opens one through `UpdateCompShareInstancePorts`, preserving existing HTTP/TCP ports. The change takes effect immediately without restarting the container. The listen port and forwarded address are saved on the node, and the checklist includes `Server-dial port`. Each instance supports at most 10 open TCP ports, including SSH. If all 10 are open and occupied, installation fails; close one under the provider console's port configuration or use an agent-initiated connection. VM instances have a public IP, so the server connects directly to the listen port (default: `:3389`), which the security group must allow.
- **AutoDL**: one-click installation does not offer this direction. If a usable TCP endpoint is available, configure it according to the provider's documentation, then connect manually through [Add Compute Nodes](connect-nodes.md).
- **Vast.ai, RunPod**: when both fields are blank, installation selects an already mapped TCP port that has no listening process and is not SSH, and records the actual listen port and public mapped address. Precheck only indicates that this is checked during installation; it does not incorrectly test the default port.
- **Vast.ai with no free mapped port**: use an agent-initiated connection or choose an instance template with spare mapped ports. Do not assume that arbitrary container ports are externally accessible.
- **RunPod with no free mapped port**: requests the restart confirmation described above. After confirmation, it adds a TCP port and waits for the new mapping and SSH to become available before installation (up to 5 minutes). If the wait fails, check the report and the pod's actual state, then retry.

Cloud one-click installation is one way to [connect nodes](connect-nodes.md). You can also install manually on the same instance using the SSH wizard or `/install.sh`.

Rented nodes have the system labels `gpuworks.ai/provider` (cloud provider) and `gpuworks.ai/region` (region). Use them to create [sync groups](configure-pull-settings.md#同步组): cloud hosts across different providers or regions usually cannot reach each other and should not copy models or images between one another.

## Stop and Release Instances to Avoid Further Charges

In GPUWORKS, you can do the following:

- **Power off**: click **Power off** in the instance row and confirm. Only types that support power control have this button (Alibaba Cloud, CompShare, and AutoDL); the same applies to **Power on**. After you click it, **Power-off request sent** appears. The subsequent sync result determines the status. Alibaba Cloud uses **normal stop**, which continues billing; powering off does not release the instance.
- **Release (destroy)**: the UI has no button for releasing instances. RunPod and Vast.ai do not support power control, so they have no power-off button. Manage stopping or destroying these instances in the provider's console, or release them with a supported REST API. Check the bill to confirm whether charges have ended.
- **Delete a cloud account** (**Delete** in the list) only deletes the account, credentials, and instance records in GPUWORKS. It **does not** stop or destroy instances in the cloud or delete onboarded nodes.
- A successful API release immediately removes the instance from the list; a failed release leaves the list unchanged. Instances destroyed in the provider's console or reclaimed after expiration are removed on the next successful sync. A failed sync (account marked **Error**) does not change the list.
- Nodes onboarded on these instances are not deleted: they remain and are marked `stopped` directly during out-of-band recovery (reason: `provider instance no longer exists`), with no further SSH recovery attempts. See [Troubleshooting](troubleshooting.md). Delete a node manually after confirming that you no longer need it.

Billing rules, including whether disks or bandwidth continue to incur charges after shutdown, are determined by each provider. Refer to the bills in their consoles. **After you finish using an instance, confirm that it is destroyed in the provider's console and review bills regularly**.

## Create and Release Instances Through the API

To place orders or release instances directly through GPUWORKS without opening the provider's console, use the REST API (with a JWT that has the required permissions or a read/write platform API token; see [API Reference](../reference/api.md) for fields):

- `POST /api/v1/providers/{id}/instances`: create an instance. The request body contains `name`, `region`, `gpu_model`, `gpu_count`, `image`, and `extra` with string key-value pairs. Success returns 201 and instance information.
- `DELETE /api/v1/providers/{id}/instances/{externalID}`: release an instance.
- `POST /api/v1/providers/{id}/instances/{externalID}/power`: use the request body `{"action":"on"}` or `{"action":"off"}`.

After creation, click **Sync** on the **External Platforms** page for the instance to appear in the list. Requirements differ by type:

| Type | Creation Requirements |
|---|---|
| RunPod | `gpu_model` is required (RunPod GPU model ID); `gpu_count` defaults to 1; `image` defaults to `runpod/pytorch:...`; `extra` can include `disk_gb` (default: 20), `volume_gb`, `ports`, and other fields |
| CompShare | `gpu_model` and `image` (CompShare image ID) are required; `gpu_count` defaults to 1; omitted `region` uses the provider's default region; `extra` can include `zone`, `disk_gb` (default: 100), `cpu`, `memory_mb`, `password`, and other fields |
| AutoDL | `image` (image UUID) is required; the GPU model must map to a known specification or be supplied in `extra.gpu_spec_uuid`; `gpu_count` is 1–4; `region` is a list of data centers |
| Vast.ai | `extra.offer_id` is required (search for and select an offer on Vast.ai first; that offer determines the price); `image` has a default value; `extra.disk_gb` defaults to 20 |
| DigitalOcean | `region` and `extra.size` (GPU size slug, such as `gpu-h100x1-80gb`) are required; when `image` is omitted, DigitalOcean's AI/ML Ready image is chosen by size (NVIDIA single GPU `gpu-h100x1-base`, multi-GPU `gpu-h100x8-base`, AMD `gpu-amd-base`); `extra.ssh_keys` can carry comma-separated account SSH key IDs or fingerprints, and the platform public key is always added |

These requirements come from each provider implementation; API error messages are authoritative. Check balance and inventory before creating an instance. For release, RunPod, Vast.ai, and DigitalOcean destroy the instance directly; CompShare and AutoDL power it off first, then release it. All of them currently return 501 (unsupported) for the `resize` API.

## Frequently Asked Questions

**Adding an account reports “Add failed: credential validation failed …”**
The provider rejected the credentials. Check that you copied the complete API key or public/private key pair (check for leading or trailing spaces or newlines), that it belongs to the correct platform, that the account is not restricted, and that the server host can access the provider API (whether **API endpoint** needs a proxy). When you edit an existing account, blank fields retain their old values, so enter the new key if it has changed.

**An account shows Error, or clicking Sync reports Synchronization failed**
Sync actually calls the provider API; a failure marks the account as **Error**. Follow the failure message to check for revoked credentials or overdue payments, then click **Sync**. A successful sync automatically restores **Valid** status.

**The instance table is empty (“No instances have been synchronized. Click Sync above and try again.”)**
You have not synced yet, or the provider account has no instances. Click **Sync** first.

**Install Agent fails**

First read the installation outcome at the top of the page:

- Green **Agent installed and connected**: installation succeeded.
- Yellow **Agent installed, with notes** or **Agent started, not connected yet**: continue checking the report's warnings; the latter also requires fixing connectivity.
- Red **Installation failed**: the outcome includes a failure reason. Address the corresponding check item.

After installation, only the **Done** button remains. Closing the page refreshes the instance list. If installation fails, the form is retained; before clicking **Try again**, you can change the connection method or enter credentials under **Advanced Settings**.

**Precheck or SSH endpoint fails**

The `precheck` row lists only checks that did not pass; when all pass, it displays `all N checks passed`. Expand **Precheck details** for the full list.

If the SSH endpoint is unreachable, the username/password/private key is incorrect, or you selected a **Terminal only** endpoint, check the credentials or choose another **Connection** for SSH. **Automatic** falls back if a direct connection fails, and warnings list the reason for each endpoint's failure.

**Installation directory failures or warnings**

Cloud one-click installation defaults to `/opt/gpuworks`. Change it under **Advanced Settings** in the installation page. Both precheck and installation check its underlying filesystem. For the seven checks, see [Installation Directory Checks](connect-nodes.md#安装目录检查). Address the items in the report:

- **Install directory persistence warning**: the default directory on a container instance may be on the container's writable layer. Deleting or recreating the instance loses the agent and cache, requiring reinstallation. When renting an instance, reserve enough container disk capacity for images.
- **ownership / filesystem warning**: persistent volumes such as `/workspace` are often network filesystems that do not allow ownership changes. Determine suitability from directory permissions and filesystem restrictions.
- **file names failure**: RunPod's `/workspace` does not allow colons in filenames, so runveil cannot run deployments there. Install the agent on the container disk; persistence alone is not a reason to choose a directory.
- **Failure caused by noexec**: programs cannot execute in the directory, so installation fails immediately.
- **Install directory path access failure**: the installation directory is under `/root`, a private home, or another ancestor that the dedicated runtime user `udocker` cannot enter, and the probe establishes that the agent runs as root and will use runveil (no usable docker or nerdctl on the instance) and the dedicated user, so every later launch would fail with `permission denied`. Installation therefore stops before uploading the agent (400 `NODE_INSTALL_DIR_UNTRAVERSABLE`), and the error names the blocked directory. Run `chmod o+x <directory>` or choose another installation directory, then retry. When it is not certain that runveil will be used, for example docker or nerdctl is present on the instance, only a warning is shown and installation continues.

To install the agent on a compatible persistent volume or data disk, enter the installation directory under **Advanced Settings** in the installation page. Installation fails without fallback if an explicitly specified directory is not writable. Fix the directory issue first, then retry.

**Other installation checks**

- **SSH authentication fails after trying only the platform key**: the instance does not yet have the platform public key. For a RunPod pod started before the server registered its public key, installation from **External Platforms** asks whether to restart to add the key. You can also provide a private key or password once under **Advanced Settings**, or authorize it on the instance as instructed by the error.
- **The provider returns no SSH endpoint**: the instance may still be starting. Click **Sync** again later.
- **agent_dial warning**: the agent has started but is not connected to the server, usually because `server_url` is unreachable from the instance. Follow the next item.

**Installation succeeds, but the node does not appear under Compute Nodes or remains offline**
For an agent-initiated connection, check whether `server_url` is reachable from the instance and inspect the agent logs. If the address is wrong, correct the connection configuration while retaining this node's identity. For a server-initiated connection, check TCP connectivity from the server to the instance's connection address, port mappings, and security groups; do not enter `server_url`. For rejected connections or disconnections after onboarding, see [Troubleshooting](troubleshooting.md).

**An instance was accidentally left running and incurred charges**
GPUWORKS does not automatically release cloud instances, and deleting a cloud account does not release them either. Destroy unused instances in the provider's console, or use the API above if needed. After destruction, delete the corresponding offline node under **Compute Nodes** in GPUWORKS.

## Related Documentation

- [Connect, Upgrade, and Uninstall Nodes](connect-nodes.md)
- [Give the Console a Public Address](expose-public-address.md)
- [Troubleshooting](troubleshooting.md)
- [Testing Guide (Cloud L4 Cases and Cleanup)](../dev/testing.md)
## Alibaba Cloud

**Credentials**: enter AccessKey ID and AccessKey Secret. AccessKeys from either the primary account or a RAM user work. A primary account's AccessKey has all account permissions. **Creating a RAM user with only the permissions below is recommended**; use its AccessKey.

Leave **Regions** blank to sync all regions in the account. If you have instances in only some regions, enter comma-separated region IDs (such as `cn-shanghai,cn-beijing`) to reduce API calls.

**Required permissions** are listed below. The JSON and CLI examples include basic sync, installation, and power-control permissions. Add `ecs:DescribeSecurityGroupAttribute` when using security group checks for server-initiated connections.

| Permission | Purpose |
|---|---|
| `ecs:DescribeRegions` | Validate credentials and enumerate regions |
| `ecs:DescribeInstances` | Sync instances; locate the instance's region before powering it on or off |
| `ecs:DescribeCloudAssistantStatus` | Check whether the instance's Cloud Assistant is online |
| `ecs:RunCommand`, `ecs:DescribeInvocationResults` | Run preinstallation checks and installation through Cloud Assistant, and read results |
| `ecs:StartInstance`, `ecs:StopInstance` | Power on and power off; omit if not needed |
| `cs:DescribeClustersV1`, `cs:DescribeClusterNodePools`, `ecs:DescribeInstanceTypes` | The “Kubernetes clusters” tab lists ACK clusters and their GPU node pools; omit if you do not connect ACK |
| `cs:DescribeClusterUserKubeconfig` | Issues a kubeconfig valid for 15 minutes when GPUWORKS applies the connect manifest to an ACK cluster; omit if you connect with kubectl only |
| `ecs:DescribeSecurityGroupAttribute` | Read inbound security group rules for server-initiated connections; omit if you do not use this check |

```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ecs:DescribeRegions",
        "ecs:DescribeInstances",
        "ecs:DescribeCloudAssistantStatus",
        "ecs:RunCommand",
        "ecs:DescribeInvocationResults",
        "ecs:StartInstance",
        "ecs:StopInstance",
        "ecs:DescribeInstanceTypes",
        "cs:DescribeClustersV1",
        "cs:DescribeClusterNodePools",
        "cs:DescribeClusterUserKubeconfig"
      ],
      "Resource": "*"
    }
  ]
}
```

GPUWORKS does not create or release instances on Alibaba Cloud, so it does not need permissions for those operations.

**Create a RAM user in the console** (for UI steps, see the [Alibaba Cloud RAM user documentation](https://help.aliyun.com/zh/ram/user-guide/create-a-ram-user)):

1. Open [RAM Access Control](https://ram.console.aliyun.com/), go to **Permissions → Policies → Create Policy**, switch to **JSON**, paste the JSON above, and save it as `gpuworks-ecs`.
2. Go to **Identities → Users → Create User**, enter a logon name (such as `gpuworks`), and select **Use permanent AccessKey to access** as the access method. Follow the prompts to confirm AccessKey creation, then **immediately save** the AccessKey ID and Secret (the Secret is shown only once).
3. In the user list, select **Add Permissions** for this user, and choose `gpuworks-ecs` under **Custom Policy**.
4. Return to GPUWORKS and add the Alibaba Cloud account with this AccessKey pair.

**Create from the command line** ([Alibaba Cloud CLI](https://help.aliyun.com/zh/cli/); run as an identity with RAM management permissions):

```bash
POLICY='{"Version":"1","Statement":[{"Effect":"Allow","Action":["ecs:DescribeRegions","ecs:DescribeInstances","ecs:DescribeCloudAssistantStatus","ecs:RunCommand","ecs:DescribeInvocationResults","ecs:StartInstance","ecs:StopInstance","ecs:DescribeInstanceTypes","cs:DescribeClustersV1","cs:DescribeClusterNodePools","cs:DescribeClusterUserKubeconfig"],"Resource":"*"}]}'
aliyun ram CreatePolicy --PolicyName gpuworks-ecs --PolicyDocument "$POLICY"
aliyun ram CreateUser --UserName gpuworks
aliyun ram AttachPolicyToUser --PolicyType Custom --PolicyName gpuworks-ecs --UserName gpuworks
aliyun ram CreateAccessKey --UserName gpuworks   # Use the AccessKeyId and AccessKeySecret in the output as the credentials
```

**Optional: RAM role**. If your enterprise keeps permissions in roles or uses cross-account authorization, grant the permission policy to the role rather than the user, leave the user with only the permission to assume the role, and enter the role ARN in **RAM role ARN** (`acs:ram::<account-id>:role/<role-name>`). The server uses the user's AccessKey to call STS `AssumeRole` and obtain temporary credentials valid for one hour. Subsequent calls are signed with those temporary credentials, which renew automatically before expiration:

```bash
ACCOUNT=$(aliyun sts GetCallerIdentity | python3 -c 'import sys,json;print(json.load(sys.stdin)["AccountId"])')
TRUST="{\"Statement\":[{\"Action\":\"sts:AssumeRole\",\"Effect\":\"Allow\",\"Principal\":{\"RAM\":[\"acs:ram::$ACCOUNT:root\"]}}],\"Version\":\"1\"}"
aliyun ram CreateRole --RoleName gpuworks --AssumeRolePolicyDocument "$TRUST"
aliyun ram AttachPolicyToRole --PolicyType Custom --PolicyName gpuworks-ecs --RoleName gpuworks
aliyun ram AttachPolicyToUser --PolicyType System --PolicyName AliyunSTSAssumeRoleAccess --UserName gpuworks
```

In this case, the user does not need `gpuworks-ecs` directly (remove it with `DetachPolicyFromUser`); all ECS permissions come from the role.

**Sync**: the instance table lists only GPU-equipped ECS instances across the account's regions (instances without GPUs are hidden). GPU model and count come from instance specifications. For running instances, the platform also checks whether Cloud Assistant is online. If so, **Cloud Assistant** appears among their connections; instances with a public IP (including EIP) also have **Direct** (`root@<public-ip>:22`). You cannot install the agent on Windows instances.

**Installation channels**

With **Automatic**, agent-initiated connections prefer Cloud Assistant installation; server-initiated connections (`server_dial`) or an offline Cloud Assistant use SSH.

**Install through Cloud Assistant**

This method requires no SSH, password, platform key, or public IP, but **the instance must be able to access `server_url`**. Without public outbound access, use a reachable server address in the same VPC or a connected network. See [No Network Connectivity Between a Node and the Server](connect-nodes.md#节点与-server-网络不通).

The server calls `RunCommand` to have the instance execute, as root, the same one-line installation command used by **Add Node** (`curl <server_url>/install.sh | sh -s -- …`). The instance downloads the agent. Preinstallation checks also run through Cloud Assistant, and the server checks whether it can connect back to the instance.

**Authentication fallback for SSH installation**

The platform tries to append its public key to the instance root user's `~/.ssh/authorized_keys` through Cloud Assistant, avoiding duplicate keys, then logs in with the platform key:

- **Cloud Assistant is online**: the public key can be added directly without manually entered credentials. The report item is `Platform key (Cloud Assistant)`.
- **Cloud Assistant is not yet online**: installation waits up to 2 minutes, then adds the key once it comes online. The report records the wait time. Just after startup, SSH may become reachable first, while Cloud Assistant sends its first heartbeat roughly a minute later.
- **Still offline after waiting**: for example, if a custom image has no preinstalled Cloud Assistant and the instance lacks the platform key, provide a password or private key once under **Advanced Settings**. The Alibaba Cloud API does not return the root password. To install Cloud Assistant, choose the method for your operating system in the [Alibaba Cloud installation instructions](https://help.aliyun.com/zh/ecs/user-guide/install-the-cloud-assistant-agent), and check whether it requires a restart.

For an instance without the platform public key, the `SSH` row in precheck displays a warning explaining that Cloud Assistant adds it during installation. Other checks requiring login continue after authentication is obtained. After installation, check the report for key-write and wait results.

**Cloud Assistant command records**

Cloud Assistant records executed commands; retention depends on Alibaba Cloud settings and documentation. The command contains the node connection token, which the agent continues using to connect to the server. Alibaba Cloud users authorized to view invocation records (`ecs:DescribeInvocations`) can read the token. Deleting the node invalidates its token.

**ACK clusters**: the “Kubernetes clusters” tab of an expanded account lists, live, the account's ACK clusters (Serverless and ACS clusters run Pods on virtual nodes, which cannot run the agent DaemonSet, so they are not listed) and their GPU node pools. A node pool counts only when all its instance types carry GPUs of the same model; auto-mode node pools (no specified instance type) are not listed. Choose a node pool and click “Connect” to create a Kubernetes cluster entry whose node selector is `alibabacloud.com/nodepool-id=<node pool ID>`. After that, GPUWORKS can apply the manifest, or you can run kubectl yourself; see [Connect Managed Kubernetes from a Public Cloud](connect-managed-kubernetes.md). The nodes of an ACK cluster are themselves GPU instances: they appear in the instance table marked “Cluster node” and cannot have the agent installed individually.

For GPUWORKS to apply the manifest, the cluster's API Server needs a public address (select “Expose API Server with EIP” when creating the cluster; clusters without one show “Private API only” and can be connected only with kubectl). If you use a RAM sub-account, that sub-account must also be granted cluster administrator RBAC permissions (or permissions to create Namespaces, RBAC, DaemonSets, and PriorityClasses) on the cluster under “Authorizations” in the ACK console; otherwise the issued kubeconfig is rejected by the cluster, and GPUWORKS shows the cluster's rejection reason verbatim.

**Power control**: power-off uses **normal stop** (billing continues), preserving resources such as the instance's GPUs and fixed public IP. **Economical mode** releases those resources, and capacity may be unavailable when restarting. GPUWORKS does not use that mode.

## DigitalOcean

**Credentials**: in the DigitalOcean console, go to “API → Tokens”, generate a Personal Access Token, and enter it as the API Key. Full Access is simplest. With custom scopes, cover read access to account, droplet, size, ssh_key, and kubernetes; write access to droplet for powering on/off, creating, and deleting Droplets; write access to ssh_key for registering the platform public key; and the corresponding kubernetes permission for issuing a DOKS kubeconfig (custom scope combinations have not been tested one by one; when a permission is missing, the UI shows the reason DigitalOcean returns).

**Sync**: the instance table lists only GPU Droplets (sizes that include GPUs); the model, GPU count, and hourly price come from the size. DigitalOcean GPU sizes must be enabled for the account; if a size does not appear in the console or API, request it in the console first. The nodes of a DOKS cluster are also GPU Droplets; they are marked “Cluster node” and cannot have the agent installed individually.

**Installation**: over SSH (`root@<public IP>:22`). When the account is added, the platform public key is registered as an account SSH key, and Droplets subsequently created through GPUWORKS carry it, so no credentials are needed. DigitalOcean cannot add an account key to an existing Droplet; for Droplets created elsewhere, provide a private key or password once under “Advanced Settings” during installation. SSH keys already in the account are never modified or deleted; the `gpuworks-platform` key registered by the platform stays in DigitalOcean after you delete the account in GPUWORKS, so delete it under “Settings → Security” in the console when it is no longer needed.

**Power control and release**: power on and power off map to the Droplet's power on / power off (billing continues after power-off); releasing deletes the Droplet together with its data.

**DOKS clusters**: the “Kubernetes clusters” tab lists the account's DOKS clusters and NVIDIA GPU node pools (AMD node pools are not listed). After connecting, the node selector is `doks.digitalocean.com/node-pool-id=<node pool ID>`. GPUWORKS can apply the manifest with a kubeconfig valid for 15 minutes, or you can run kubectl yourself; see [Connect Managed Kubernetes from a Public Cloud](connect-managed-kubernetes.md).
