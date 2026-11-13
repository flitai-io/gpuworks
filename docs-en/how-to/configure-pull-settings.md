# Configure Image and Model Pulls: Source Rules, Proxies, Sync Groups, and Credentials

This guide explains how to configure model and image source addresses, outbound proxies, node copy scope, and access credentials, and how to verify that the configuration takes effect on the target nodes.

**Use cases**: Slow or failed downloads, internal mirrors, outbound access through a proxy, private models, or restrictions on copying between data centers.

**Prerequisites**: Administrator permissions are required to change pull settings. Prepare the addresses for source rules or proxies. To apply different rules to different nodes, first assign the corresponding labels to the nodes. The target node must be online to verify node behavior. To verify management node outbound access, that machine must also be able to reach the relevant addresses.

Open **Settings → Image & model pulls** and choose an entry point for your goal:

| Problem to Solve | Configuration Entry Point |
|---|---|
| Retrieve files from a mirror, Harbor, or an internal model proxy | [Source rules](#来源规则) |
| Access external addresses through an HTTP proxy from a node or the management node | [Proxies](#代理) |
| Control which nodes automatically copy from each other and limit copy rates | [Sync groups](#同步组) |
| Authenticate to private images or gated models | [Credentials](#凭据) |

When a deployment starts, scales out, or is rebuilt, it first uses complete copies in local or readable shared storage, then tries copying from nodes in the same group, and finally pulls or downloads according to the rules. **Download to…** on the Models page explicitly downloads from the model's registered source. To manually use an existing copy, choose **Copy to…**; see [Download and Sync Models](download-and-sync-models.md). Changing rules does not replace existing files.

## Behavior Without Configuration

- System images in the image catalog are published to Docker Hub, quay.io, and Huawei Cloud SWR. The node probes `https://<host>/v2/` on these registries in parallel (a 5-second timeout, results cached for 30 minutes, through the same proxy as the pull), then tries the reachable registry with the lowest latency first.
- A template's `image_sources` declares other copies of the same image. These come before catalog copies and are also ranked by the node.
- If an image download has reported byte progress and is not yet complete, 3 consecutive minutes without progress count as a stall, and the node switches to the next candidate. A slow download that still makes progress is switched away from only when it stays very slow: if it averages under 64 KiB/s over any 5 consecutive minutes and another candidate remains, the node abandons it and tries the next one. The last candidate is never abandoned for its speed. Runtimes that show byte progress only in coarse steps (large nerdctl layers) are not judged on speed.
- The native pull on runveil (udocker) nodes decides for itself how many connections to open and deals with slowness at the connection level; see [Limit Connections for Image Pulls](#限制镜像拉取的连接数runveil-节点). Each registry host has a connection budget that starts at 4 connections and grows to at most 16 only while more connections still raise the overall rate; a 429 or 503, or a refused connection, halves it and waits out `Retry-After`. A connection that delivers no data for 2 minutes, or falls far behind the other connections of the same pull for two windows in a row (with an absolute floor of 16 KiB/s over 30 seconds), is replaced by a new one that resumes from the bytes already received instead of re-downloading the layer. Each download segment uses its own TCP connection (HTTP/1.1), so one slow connection does not hold back the others.
- A registry with a network failure, transfer failure, or stall enters a 30-minute cooldown on that node. It is placed last among the probe candidates but is not removed. A missing image or denied authentication or permissions also causes a switch to another candidate, but does not put the entire registry in cooldown. An unavailable runtime ends the current pull immediately.
- By default, models download from their registered site (Hugging Face or ModelScope). Nodes connect directly, with no proxy configured (the node's own environment variables such as `HTTP_PROXY` still apply). Models with a directory URL source download only from their own directory.
- A preset sync group, `default-all-nodes`, includes all nodes: nodes automatically copy images and models from each other. If two nodes cannot connect (for example, two cloud containers are each behind NAT), the copy fails within about 10 seconds. That pair of nodes does not retry for 3 hours, and the replica pulls the files itself instead.

Reachability and latency probes do not guarantee actual download speed. Use download progress on the target node as the final measure. Docker usually reports per-layer byte progress. The 3-minute stall check above does not apply when there is no byte progress, during unpacking, or during image registration after all bytes have arrived. Each candidate has a maximum of 60 minutes from the start of the pull through the end of unpacking.

- If Docker cannot allocate a pseudoterminal, it provides no byte progress and is subject only to the total time limit for a single candidate.
- nerdctl (including Kubernetes nodes that pull through nerdctl) converts its redrawn progress table into per-layer byte progress. Once all layers are downloaded and only the elapsed time changes, the operation is treated as unpacking. Layers larger than 1 GiB report progress in increments of 0.1 GiB. The stall timeout is extended to about 17.5 minutes, the time needed to advance one increment at 100 KiB/s. Slower downloads are subject only to the total time limit for a single candidate.
- udocker does not output byte counts. The agent obtains progress from per-layer file sizes.
- A pull message of `Progress output not understood, stall check off: …` means that the runtime output format is not recognized. Stall checks are disabled for this pull, and only the total time limit applies. The agent log retains the raw line.

## Source Rules

1. Open **Settings → Image & model pulls → Source rules** and add a rule. A **Match** without a protocol is an image registry or image prefix. A match with `http://` or `https://` is a model site. Fill in the fields according to the corresponding table below.
2. Select **Applies to nodes** and enter alternative addresses in order. Turn off **Allow upstream** if connections to the original site are not allowed. For model rules, also decide whether to enable **Used by the management node for model metadata**.
3. For a site rule, use [Test connectivity](#测试连通) to check the draft before saving. After saving, start the required download or pull on the target node and check the actual source and completion status. A successful test only means that the probe request succeeds; it does not guarantee that the full files can be downloaded.

### Image Rules

Each rule has these fields:

| Field | Meaning |
|---|---|
| Match | A registry host (`docker.io`, `ghcr.io`, `registry.example:5000`) covers all images in that registry. A reference prefix (`gpuworks/`, `quay.io/team/app`) covers images under that prefix. |
| Alternative addresses (in order) | An ordered list. Host rules preserve the repository path: `docker.io` → `harbor.corp.example/dockerhub-proxy` turns `nginx:1` into `harbor.corp.example/dockerhub-proxy/library/nginx:1`. Prefix rules replace the prefix. |
| Allow upstream | Whether to try the image's original registry after all alternative addresses fail. Turn this off for a strictly internal network. |
| Applies to nodes | A node label selector. Empty means all nodes. Rented cloud hosts come with `gpuworks.ai/provider` and `gpuworks.ai/region` labels. |

All matching rules that apply to a node are combined in list order: alternative addresses come first, followed by the image's own copies (ranked by the node's probes). A rule with no alternative addresses and upstream disabled forbids pulls from that registry. If all candidates are forbidden, startup fails with an explanation.

After pulling from an alternative address, the platform adds the original image name on the node, so the deployment continues to use the original reference. Rules apply only to pulls initiated by the platform. A manually executed `docker pull` does not use these rules. Configuring rules does not modify `daemon.json` or containerd's `hosts.toml`.

### Common Rule Examples

| Scenario | Rule |
|---|---|
| Internal nodes must use a self-hosted Harbor proxy cache | `docker.io` → `harbor.corp.example/dockerhub-proxy` and `quay.io` → `harbor.corp.example/quay-proxy`, with upstream disabled and `site=idc-a` as the node selector. Configure credentials if Harbor requires a login. |
| Your company also publishes a copy of a system image internally | `gpuworks/` → `registry.corp.example/gpuworks/`, with upstream allowed. |
| A template's image is available only on ghcr | `ghcr.io` → an available ghcr mirror, with upstream allowed; or ask the template author to declare `image_sources`. |
| You want to use a community Docker Hub accelerator | `docker.io` → the accelerator address, with upstream allowed. The platform includes no built-in accelerators. Check access restrictions and availability before use. |

### Site Rules (Models)

Set the match to a site root without a path, such as `https://huggingface.co` or `https://modelscope.cn`. Fields:

| Field | Meaning |
|---|---|
| Match | A site root, such as `https://huggingface.co`. The protocol and hostname are required; no path is allowed. |
| Alternative addresses (in order) | An ordered list of `http(s)://` addresses, optionally with paths, such as `https://hf-mirror.com`, `https://xget.example/hf`, or `http://hf-proxy.corp:8090`. The node redirects requests for the original site to these addresses. |
| Allow upstream | Whether to connect to the original site after all alternative addresses fail. Turn this off for a strictly internal network. |
| Applies to nodes | A node label selector. Empty means all nodes. |
| Used by the management node for model metadata | When selected, the management node (the machine running the server) can use this rule's addresses to read model metadata. This option is available only for site rules. |

**Rules are combined**: When a node downloads a model from a site, all matching rules add their alternative addresses to the candidate list in rule order. Duplicates are removed, and the original site comes last. If any matching rule disables upstream, the original site is excluded. If no candidates remain, downloading is forbidden (`WEIGHTS_FETCH_FORBIDDEN`).

Models with a directory URL source (registered with an HTTP directory address) download only from their own directory; these rules do not apply to them. ModelScope and Hugging Face do not fall back to each other. A model is sought only through the rules for its registered site and the site itself.

#### Public Mirrors and Internal Proxy Examples

For the match `https://huggingface.co`, enter alternative addresses according to the node's network:

- **Use a public mirror**: Enter `https://hf-mirror.com` and turn on **Allow upstream**. The node tries hf-mirror first and falls back to huggingface.co after a failure or two stalls at the same address.
- **Use an internal HF proxy**: Enter `http://hf-proxy.corp:8090`. You can also place `https://hf-mirror.com` after it. If only internal access is allowed, turn off **Allow upstream** to exclude huggingface.co from the candidates. Alternative addresses are still tried in the order entered.

The same applies to internal image registries: in an image rule, point `docker.io` to `harbor.corp.example/dockerhub-proxy` and turn off **Allow upstream**.

When you use both rented hosts outside mainland China and data centers in mainland China, restrict **Applies to nodes** for these model rules with labels such as `site=idc-sh` or `gpuworks.ai/region`. This lets overseas nodes connect directly and mainland nodes use mirrors. Rented cloud hosts come with `gpuworks.ai/provider` and `gpuworks.ai/region` labels.

#### How Nodes Try Candidates in Order

The Models page shows the current download site, with a reason when the site changes. Models pinned to a commit can resume downloads when the address changes. Models that use a branch such as `main` start over when switching addresses.

The server calculates candidates for each node, with rule alternatives first and the original site last. When retrieving the file list, an unreachable address, HTTP error, or unparseable list puts the current address in cooldown for 30 minutes and causes the node to try the next candidate.

When downloading individual files:

- **Retryable transfer errors**: Connection refusals or resets, TLS handshake timeouts, EOF, and similar errors are retried at the same address first. Each file gets up to 6 attempts per address, or 16 at the last address, including the initial attempt. Only after all attempts fail does the node put the address in cooldown and switch.
- **Non-retryable source errors**: HTTP error statuses (including 404, 401/403, and 429), TLS certificate verification failures, and similar errors immediately put the address in cooldown for 30 minutes and trigger a switch to the next candidate. If there is no next candidate, the download ends.
- **Consecutive stalls**: Five consecutive minutes without response headers or new data count as one stall. The connection is terminated and a resume is attempted. Two consecutive stalls at the same address trigger cooldown and a switch if another candidate is available. The last address is not switched out and remains subject to the limit of 16 total attempts.

An address in cooldown is placed last the next time, but is not removed. Cooldown is shared with image pulls.

#### How the Management Node Reads Metadata

If the management node and compute nodes have different networks, select **Used by the management node for model metadata** on site rules available to the management node. If it needs a proxy, also select **Used for management node outbound access** on the proxy. The management node probes these site rules' candidate addresses one by one, allowing up to 5 seconds per candidate, then reads metadata from the selected address. It accesses the original site only if upstream is allowed. If there are multiple Hugging Face-style candidates, an address that allows repository information to be read but returns 401/403 for files is skipped in favor of other candidates. If every candidate denies file access, the management node still attempts to read metadata from the first address that responds. This does not establish that weights downloads are authorized.

Outbound requests for directory URLs, image archives, login tests, and similar operations use the management node proxy and are not rewritten by site rules.

The management node needs no allowlist by default to access ordinary private model sites, HTTP directories, image registries, or local loopback destinations. When strict mode is enabled, the necessary hosts or network ranges must be authorized; in that mode, a configured proxy or network reachability alone is not sufficient. Platform administrators can manage the switch and allowlist under **Settings → Security & access → Outbound access policy**; host administrators can also edit `outbound_restrict_private_hosts` and `outbound_allow_private_hosts` directly in `server.yaml`. Changes to either field apply to the next management-node HTTP request or redirect after saving, without restarting the server; response bodies already being read are not interrupted. Saving the page writes back to the same configuration file. If another page or a file edit has changed it, reload before saving to avoid overwriting another administrator's changes. A missing, unreadable, invalid, or over-1-MiB configuration file temporarily denies ordinary private and local loopback destinations while public access retains its existing policy; repairing the file restores the valid policy on the next request. Special metadata, link-local, and reserved addresses remain prohibited. See [Outbound HTTP Proxy Policy](../reference/configuration.md#出站-http-代理策略).

| Registered Source | If Reading Fails |
|---|---|
| Hugging Face / ModelScope | Registration is still allowed, but metadata may be missing. If the page shows `MODEL_METADATA_UNREACHABLE` or `MODEL_METADATA_FORBIDDEN`, check management node rules and outbound access. Successful registration does not mean the model is ready. |
| HTTP directory URL | The management node must first read and parse the directory. Failure blocks registration. Site rules do not rewrite the directory address, and the management node proxy controls outbound access. See [Import from an HTTP Directory](import-from-http.md). |

The browser can also read Hugging Face metadata from the same set of management node candidate addresses. It is marked as read by the browser only if content is actually retrieved; otherwise, the management node tries. Temporarily unavailable metadata on a public site does not by itself cause deployment admission to fail. Model files and other deployment requirements must still be satisfied.

## Common Network Configuration Examples

The following table shows how to configure different network environments. You must enter all addresses yourself. The platform has no built-in mirrors, accelerators, or presets.

| Network Scenario | Where to Configure |
|---|---|
| A. Overseas nodes or nodes with direct public internet access | No configuration needed; use the [default behavior](#不配置时会怎样). |
| B. Mainland China, using a public mirror | Enter the model mirror under [Source rules](#来源规则). |
| C. A data center with an internal HF proxy | Enter the internal model proxy and image registry under [Source rules](#来源规则), and disable upstream as needed. |
| D. Rented overseas hosts and mainland data centers | Use the node selector in [Source rules](#来源规则) to distinguish data centers. |
| E. Fully offline | Preload models, or configure internal model sites and disable upstream. Prepare images separately; see [below](#完全离线两条禁止规则). |
| F. Gated or private models | Add [credentials](#凭据) for the corresponding site. |
| G. Different networks for the management node and compute nodes | Configure [management node metadata access](#管理节点怎样读元数据) and the [management node proxy](#代理) separately. |
| H. Nodes require a proxy for outbound access | Configure [proxies](#代理) by label. For Docker nodes, also verify that the settings take effect. |
| I. Offline import | Register models as described in [Import Models Offline](#离线导入模型) below. If the entire cluster is offline, also configure the two blocking rules. |

### About hf-mirror.com

hf-mirror.com is a public mirror run by an individual, with no SLA. It may throttle traffic, change its implementation, or go offline, so the platform does not use it as a default. You must enter it manually for scenario B above. For production environments, use a self-hosted proxy (such as xget) or an internal cache such as Harbor or Nexus as the first alternative address, with hf-mirror and the original site as fallbacks.

### Fully Offline: Two Blocking Rules

In a cluster without outbound access, add the following two rules to forbid downloads from Hugging Face and ModelScope and avoid waiting for public internet connection timeouts. These rules are not a platform-wide switch to disable networking.

| Match | Alternative Addresses | Allow Upstream | Applies to Nodes | Used by the Management Node for Model Metadata |
|---|---|---|---|---|
| `https://huggingface.co` | Empty | Off | Empty (all) | Selected |
| `https://modelscope.cn` | Empty | Off | Empty (all) | Selected |

Models from these two sites must then use existing node directories, shared storage, or copies on nodes in the same sync group. If no usable source exists, the deployment page explains why. If the internal network has a mirror for the model, enter it as an alternative address.

HTTP directory models still download from their registered directory addresses and can use internal file servers. Image registries require separate image rules, internal copies, or offline archives. For the complete preparation procedure, see [Deploy in an Offline Environment](deploy-offline.md).

### Import Models Offline

1. Copy the model directory to a node (or shared storage).
2. Choose either registration method: register it as a local path (source local), or register a Hugging Face / ModelScope model first (so inference templates can find it by source), then use “Register path” for that node on the Models page.
3. In the model details, confirm that the selected node and path are available. Other nodes in the same sync group can copy this model from that copy when needed. For manual operations, see [Peer Sync](peer-sync-models.md).

**Register path** fills in missing metadata (config, README, and size) without overwriting existing content. If the directory was downloaded with `huggingface-cli download --local-dir`, all `.cache/huggingface/download/*.metadata` files point to the same commit, and this is the model's only copy, the model is pinned to that commit.

## Test Connectivity

1. Click **Test connectivity** in the site rule page. Select 1 to 5 target nodes and a repository; the default is `Qwen/Qwen3-0.6B`. To verify a gated model, enter its actual repository so that you do not test only a public repository.
2. Review results for each node and address: Usable, Needs credentials, an HTTP status code, or Unreachable. A 401/403 response means you need to check credentials for that site; it does not mean the address is fully usable.
3. Correct failures, test again, and save. Then confirm the configuration by downloading on the target node. This test uses the node's proxy, `no_proxy`, and credentials. It does not verify the management node's own connectivity.

The test shows results for each node and address separately: **Needs credentials** means the address is reachable but file downloads are denied with 401/403; **Unreachable** means the probe did not succeed, with the latency or reason included. Each address has a maximum wait of 10 seconds. Readable public repository information on Hugging Face does not mean gated files can be downloaded, so the test also tries to read a configuration file. A repository without `config.json`, such as a GGUF repository, does not fail the reachability check for that reason alone.

<details>
<summary>Probe Requests and API</summary>

Each address has a 10-second timeout. Probes work as follows:

- Hugging Face-style addresses: `GET <address>/api/models/<repository>/revision/main`, followed by the first byte of `<address>/<repository>/resolve/main/config.json` after a response. Gated repositories expose repository information to everyone and restrict only file downloads. To test a gated model, use its repository. If file access is denied (401/403), the result shows “Needs credentials”. A missing config.json (such as in a GGUF repository) does not affect the conclusion.
- modelscope.cn: `GET <address>/api/v1/models/<repository>`.

The endpoint is `POST /api/v1/pull/source-rules/test`.

</details>

## Sync Groups

Sync-group names must be unique after trimming, are case-sensitive, and are limited to 64 UTF-8 bytes for both create and rename. The list and delete confirmation show `#ID`. Upgrades deterministically suffix historical duplicate names by ID while preserving every group ID, node selector and rate limit.

Add or edit a group under **Settings → Image & model pulls → Sync groups**, use node labels to define its scope, set a copy rate limit, and save. Then check that the source node matches your expectations in the deployment's acquisition estimate or actual copy progress. Nodes automatically copy images and models only from nodes in the same sync group:

- A node in multiple groups can copy from any node in those groups. If two nodes share multiple groups, the strictest rate limit applies.
- A node that belongs to no group does not copy automatically. You can only initiate copies manually from the Models or Images page.
- Sync groups express “reachable, but should not be used”: for example, two data centers connected by a slow dedicated link, or two machines with public IPs on different continents. Unreachable nodes do not need separate groups; copying fails quickly and falls back.
- The default group is an ordinary group: you can rename, narrow, or delete it. In a cluster spanning multiple data centers, delete it and create one group per data center using labels. Rented cloud hosts can be grouped by `gpuworks.ai/provider` or `gpuworks.ai/region`. To restore copying among all nodes, create a group with no node conditions.
- Multiple values for one label match any of those values; different labels must all match. To select specific nodes (such as your own machines without cloud provider labels), first add a label to them on the Nodes page.
- Each group can have a copy rate limit. The default is unlimited.

The network used for copying is still selected according to [Storage Sync Networks](configure-networks.md). Nodes on the same storage sync network are preferred as sources.

## Credentials

Add entries under **Settings → Image & model pulls → Credentials** according to the table below, with one entry per host. After saving, use **Test login**, then verify with an actual pull on the target node. Passwords and tokens are stored encrypted and are never returned by the API or displayed again on the page.

| Type | What to Enter | How It Is Used |
|---|---|---|
| Image registry | Registry host, username, password or token | Sent with the node's desired state and held only in agent memory. A temporary `DOCKER_CONFIG` directory is written for the pull and deleted afterwards. The platform does not run `docker login` or touch `~/.docker/config.json` on the node. |
| Model site | Site host (such as `huggingface.co`) and token; leave the username empty | Sent as `Authorization: Bearer`, **only over https with an exact host match**: mirrors and other domains never receive it. On redirects, the same domain and its subdomains (such as the site's CDN) receive it; other domains do not. |

- udocker's native pull includes authentication directly. The udocker command-line fallback does not support authentication and pulls anonymously.
- **Test login** runs from the management node through the proxy marked **Used for management node outbound access**. Private-network addresses must also meet the [management node authorization requirements](#管理节点怎样读元数据). Image registries are tested at `/v2/`, and tokens with Hugging Face's `GET /api/whoami-v2`. A site reachable by a node but not by the management node fails the test; use actual pulls as the final measure.
- Only Hugging Face private and gated repositories have been confirmed to work so far. ModelScope private repositories have not been verified.
- The endpoint is `/api/v1/pull/credentials`; the old `/api/v1/pull/registry-credentials*` endpoints return 410.

## Proxies

Add an entry under **Settings → Image & model pulls → Proxies**, fill in the fields below, and save. Use **Preview the proxy a node uses** to confirm the match scope, then open the target node details and check whether **Pull proxy** is applied. For Docker, also follow the [dockerd](#dockerd) status guidance below.

| Field | Meaning |
|---|---|
| `http_proxy`, `https_proxy` | Proxy addresses |
| `no_proxy` | Destinations that bypass the proxy, with the same semantics as Go's `NO_PROXY`: `*` means all destinations; IPs and CIDRs are supported; a domain matches itself and its subdomains; a leading `.` or `*.` matches only subdomains; ports are supported; loopback and localhost always bypass the proxy. |
| User name, Password | The password is stored encrypted and is never displayed again. |
| Applies to nodes | A node label selector. Empty means all nodes. |
| Note | Free text |
| Used for management node outbound access | When selected, the management node uses this entry for outbound access. |

For example, if data center nodes require a proxy for outbound access, restrict **Applies to nodes** to `site=idc-sh` and fill in `http_proxy`, `https_proxy`, and `no_proxy`. Matching nodes use this proxy for image pulls, model downloads, and probes. If the management node also needs the proxy, select **Used for management node outbound access**.

A node uses the **first** entry whose conditions it satisfies. Entries are not combined. If none match, the agent uses its own environment variables. The management node's outbound proxy (for reading metadata, fetching logos, registering directory URLs, previewing and probing image archive URLs, and testing credential logins) is the first entry with “Used for management node outbound access” selected.

For its own requests, the agent also adds loopback and the server address to `no_proxy`. The same list is used for the agent's own requests, nerdctl and udocker command-line pulls (which previously dropped `NO_PROXY`), and the dockerd drop-in. Models with directory URL sources now also download through the node proxy. Add internal file servers to `no_proxy`.

The node details include a read-only “Pull proxy” card that shows the proxy entry used by the node and the dockerd status. The Proxies page has “Preview the proxy a node uses”, which lists the entry each node uses based on saved configuration. Endpoints: `/api/v1/pull/proxies` and `/api/v1/nodes/{uuid}/pull-proxy`.

### dockerd

nerdctl and udocker nodes apply node proxy settings on every pull, so changes take effect immediately.

On Docker nodes, dockerd performs pulls, so the proxy must be configured on dockerd. The agent writes the proxy to a systemd override file and runs `systemctl daemon-reload`, but **does not restart** dockerd. After saving a proxy in the platform, verify it in this order:

1. In the **Pull proxy** card on the node details page, check the proxy dockerd actually uses. If it differs from the settings, the page shows **Not applied**.

   After you clear the platform proxy, the agent deletes the proxy override file it generated. Until dockerd restarts, it still uses the old proxy and the page continues to show **Not applied**. If no platform proxy is configured for the node, the platform does not modify your own dockerd proxy configuration; it only displays the current value.

2. If a restart is needed, first check its impact: if dockerd has live-restore enabled, containers continue running. Otherwise, all containers on the node stop, and the platform restarts inference instances. Once you confirm that this impact is acceptable, click **Restart dockerd** next to the card and confirm.
3. Return to the **Pull proxy** card to verify that the proxy is applied. The page compares the currently loaded configuration with dockerd's actual report; saving settings successfully is not enough.

<details>
<summary>dockerd Override File and Configuration Comparison</summary>

The override file is `/etc/systemd/system/docker.service.d/zz-gpuworks-proxy.conf`:

```ini
[Service]
Environment="HTTP_PROXY=http://192.0.2.8:3128" "HTTPS_PROXY=http://192.0.2.8:3128" "NO_PROXY=localhost,127.0.0.1,::1,<server-address>,<your-no_proxy-entries>"
```

The filename starts with `zz-` so that it comes after common manually created override files such as `proxy.conf` and `http-proxy.conf`. systemd applies environment variables with the same name in filename order. Proxy settings in `daemon.json` may still take precedence; see the table below. The comparison reads the configuration currently loaded by systemd (`systemctl show docker -p Environment`) and `docker info`.

</details>

Troubleshooting:

| Symptom | Cause and Resolution |
|---|---|
| Always shows Not applied, with no restart button and a message that `daemon.json` takes precedence | `proxies` is configured in `/etc/docker/daemon.json`, and dockerd gives it priority. Remove that proxy (or change it to the value you want) and restart dockerd. |
| Shows that root is required | The agent is not root and has no passwordless sudo (`sudo -n true` fails). Manually use the file content and two commands provided on the page (`sudo systemctl daemon-reload` and `sudo systemctl restart docker`). The page compares against `docker info` and automatically shows Applied after execution. |
| dockerd is not managed by systemd, or uses rootless Docker or Docker Desktop on Mac | The platform only displays dockerd's proxy; it does not modify it. |
| The proxy includes a username and password | dockerd displays both as `xxxxx` in `docker info`. The comparison handles them the same way. |

## Limit Connections for Image Pulls (runveil Nodes)

A runveil (udocker) node downloads image layers itself. How many TCP connections it opens to the registry at once is now decided automatically instead of being fixed at layers times segments (up to 24). Private registries (Harbor, Huawei Cloud SWR, Alibaba Cloud ACR, registry.zstack.ai) and corporate proxies often limit connections or request rate per source address, and too many connections draw 429 responses or dropped connections. When the link itself is slow, more connections are not faster either. Pulls by docker and nerdctl are done by their own processes and are not affected here.

- **One budget per registry host.** Images pulled at the same time on one node share the budget of the same registry. Through a proxy, the budget belongs to the proxy and is shared by every registry behind it.
- **Start at 4 connections and add more only when they pay.** Every 3 seconds the node measures the overall rate of the pull. After doubling, the first window, in which the new connections are still being set up (TCP, TLS, redirects), is not judged; if the next window shows a rate gain of less than 15%, it goes back to the previous number and tries once more before settling there. Layers larger than 256 MiB are cut into at least 4 segments, and larger ones into segments of about 64 MiB (at most 64), so a layer can be read over more segments at once as the number of connections grows. The default ceiling is 16 connections.
- **Slow down when the registry asks.** On a 429, a 503 or a refused connection the number of connections is halved; 3 connection resets in one window, or half of the open connections, also halve it (held for at most a minute before it may rise again), down to 1. A burst of simultaneous refusals counts once and waits only one pause. A refused connection is waited on and retried, not failed at once. If the registry sends `Retry-After`, the node waits that long (at most 2 minutes) before the next request. If it does not, the node waits 2 seconds and doubles the wait each time, up to 30 seconds, and never reconnects immediately. Other pulls from the same registry wait as well. A request waits through at most 5 consecutive refusals; if it is still refused, the pull fails and candidate switching tries the next source.
- **One connection per layer when Range is not supported.** If the registry answers a segment request of a layer with the whole file, that layer is read whole over a single connection; other layers are not affected.
- **Slow connections are replaced by a relative standard.** A connection is replaced with a new one that resumes from the bytes already received if, for two 30-second windows in a row, it averages under 16 KiB/s (the absolute floor), or stays under one tenth of the median rate of the other connections of the same pull, or drops to one tenth of its own earlier peak while the other connections do not. If a replacement is replaced again before receiving a single byte, five times in a row, the layer is given up on. When the whole link is slow, the connections run at similar speeds and none is judged slow. At most half of the connections are replaced in one window, so they are never all cut at once.
- **Nothing is remembered.** Every pull starts again at 4 connections; the node does not store learned limits.

If a registry or proxy explicitly allows only a few connections, cap the connections per registry host with the agent flag `--image-pull-max-connections N` (or the environment variable `GPUWORKS_IMAGE_PULL_MAX_CONNECTIONS`). `1` means a single connection; `0` or unset means automatic (up to 16), and the largest allowed value is 32. A value out of range makes the agent refuse to start. This is a switch for unusual networks and is not needed day to day. See the [agent command line](../reference/cli.md#gpuworks-agent-run). For troubleshooting, see [Image Pull Failures](troubleshooting.md#仓库限流或连接被拒).

## Estimates on the Deployment Page

When nodes are specified (manual placement on a single node or multiple nodes), or automatic scheduling has selected nodes, the page shows an estimate for each node: what is already present, which node to copy from, which sources to pull from, and where the model comes from. The pull column lists all candidates, for example, “Pull from quay.io / swr.cn-southwest-2.myhuaweicloud.com / docker.io; the node's probe picks the source”. With source rule alternatives, it shows “Pull from harbor.corp.example first, then choose from … based on probe speed if it is unreachable”. This is an estimate based on the current state; it is reevaluated when the replica starts.

During downloads, the current source appears in several places: node rows on the Images page show **Pulling from X** or **Syncing from node Y**; the replica startup status shows `pulling <actual-pull-address>`; and the Models page and model preparation in deployment details show “Downloading from `<host>`” (with a reason after switching) or **Copying from node Y**.

Replicas in the same deployment may pull different contents for the same tag from different sources (because the tag was updated or a mirror cached old content). Deployment details show the image ID for each replica, highlighting IDs that differ from other replicas in orange.

The weights column shows “Download from hf-mirror.com → huggingface.co in order” (the candidate order calculated from the rules). If rules forbid all candidates, it shows “Cannot download: …”, with a reason suggesting that you register the path on a node that has the model or place the model in shared storage. The scheduling reason for such a deployment is `weights_fetch_forbidden` (the node can obtain the model only by downloading, but downloading is forbidden). The download endpoint on the Models page returns 409 `WEIGHTS_FETCH_FORBIDDEN`. Changing rules triggers reevaluation of existing deployments.

## Upgrade from Earlier Versions

Configuration migrates automatically during an upgrade. No manual steps are required:

| Old Configuration | Migration Result |
|---|---|
| Global URL rewrites (`network` setting) | Site rules that apply to all nodes, allow upstream, and have “Used by the management node for model metadata” selected. |
| Rewrites in node network overrides | Site rules with “Applies to nodes” restricted to `gpuworks.ai/node=<node-uuid>`, placed first. In earlier versions, a node override completely replaced the global setting. Rules now combine, so the node tries its own address first, then global addresses, and finally the original site. For example, with global hf-mirror and an internal proxy override for one node, that node's candidates are `[internal-proxy, hf-mirror, huggingface.co]`. |
| Rules you added to `image_registry_rewrites`, and non-URL rewrites in node network settings | Image rules (node overrides are restricted with `gpuworks.ai/node`). Installations whose original settings include the internal registry registry.zstack.ai receive an additional `gpuworks/ → registry.zstack.ai/gpuworks/` rule, which you can edit or delete. |
| Global proxy | One proxy entry that applies to all nodes, with “Used for management node outbound access” selected. |
| Node proxy override | A proxy entry restricted to that node, merged with global values and placed first (the node continues to use exactly the same proxy as before). |
| `user:pass@` embedded in a proxy URL | Split into a username and an encrypted password. |
| Manually selected endpoints and proxy columns for each download | Removed. |
| “Registry credentials” | Renamed “Credentials”, with existing entries classified as image registry credentials. |

Proxies are migrated before the server starts serving requests. On success, old network settings are deleted. On failure, they are retained and migration is retried on the next startup.

Behavior changes:

- The “Network” card on the Settings page, “Network override” in node details, `/settings/network`, `network:<id>`, `/nodes/{uuid}/network` (now returns 410), and network presets (hf-mirror and xget presets and DNS suggestions) have all been removed. No presets remain.
- Models with directory URL sources previously connected directly without a proxy. They now download through the node proxy. Add internal file servers to `no_proxy`; otherwise, requests are sent through the proxy.
- Model downloads previously retried the same address up to 16 times. They now switch addresses as described above when candidates remain; only the last address retains the 16-attempt limit.
