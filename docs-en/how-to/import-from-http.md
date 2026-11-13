# Import Models and Images from an HTTP File Server

Register a model directory or a `docker save` image archive on an HTTP file server with the platform, then acquire it on a node for deployment. Model directories must provide a parseable directory listing page, such as one served by Apache, nginx, or Caddy. Images use a direct URL to the archive file.

Before you start, confirm the following:

- Use an account with `managementAdmin` permission. When downloading or importing to a node, the target node must be online. Model downloads also require an agent that supports HTTP directory sources (see the error handling below for outdated agents).
- The URL uses `http` / `https` without embedded credentials. For models, it points to the directory containing the weights; for images, it points to the archive file.
- **The server can read the source** for parsing. Ordinary private and local loopback destinations need no allowlist by default. An allowlist is needed only in strict mode. Platform administrators can save authorization under **Settings → Security & access → Outbound access policy**; host administrators can also edit `outbound_restrict_private_hosts` and `outbound_allow_private_hosts` directly in `server.yaml`. Changes apply to the next request after saving, without restarting the server. See [Outbound HTTP Proxy Policy](../reference/configuration.md#出站-http-代理策略).
- **The target node downloads models**, so the node must also be able to access the directory. Image archives can be downloaded directly by the node or relayed through a server that can read the archive.

Up to 5 redirects are allowed. Each redirect for a model directory must also remain on the host used at registration. The management node and agent each use their respective pull proxy. If internal addresses need to bypass the proxy, add them to `no_proxy` in the corresponding proxy entry.

## Register a Model from an HTTP Directory

1. On the Models page, click **Register model**, select **HTTP directory** as the source, enter the URL of the model's own directory, such as `https://models.example.internal/Qwen/Qwen3-0.6B`, and click **Parse**.
2. Check the name, file count, and model metadata. The **Model ID** defaults to the last two segments of the URL. If needed, change it to the model's catalog ID; it affects inference catalog and template parameter preset matching. You can still register a model without a match and select parameters manually when deploying.
3. Confirm registration, then check that the source on the Models page is **HTTP directory**. Registration only creates a record; it does not mean the weights have reached a node.
4. To prepare weights in advance, click the model's **Download to…**, select a target node or storage, and start the download. Wait for that target copy to become ready. Alternatively, acquire the weights during deployment according to the rules for local availability, copying within a sync group, or downloading. See [Download, Cache, and Sync Models Between Nodes](download-and-sync-models.md).

Directory URL sources download only from their own directories; site rules in pull settings do not apply to them. Node downloads still use the node's pull proxy. Directory sources have no revision concept. To retain a reproducible version, point to a directory that remains unchanged after publication.

In strict mode, select **Automatically add this host to the allowlist** beside the directory address. The current host is appended only when you parse or confirm registration. Existing entries are preserved; changing the hostname requires selecting it again. Reserved addresses cannot be authorized. This permission also applies to other management-node source requests, and a failed registration does not revoke host permission that was explicitly saved.

**Fetch metadata again** for an existing model, community repository cards, and **Test login** in the credential list also offer authorization within the current operation. Authorization is offered only when global strict mode is enabled and the target needs permission. With restrictions off or a target already allowed, the original operation runs directly without an allowlist write. A Hugging Face metadata refresh successfully read by the browser does not request additional management-node source permission.

### Parsing Results and Directory Limits

Parsing reads the directory listing page on the source server, subject to these rules:

- The platform does not require a specific directory layout. If `config.json` exists, it supplies the architecture, precision, and context length. You can also register without it (for example, with only GGUF shards); these metadata fields are empty, and weight size is calculated from the GGUF files in the directory. An empty directory or a failure to parse `config.json` returns `HTTP_DIRECTORY_NOT_A_MODEL`.
- The listing page's directory size is a human-readable value with two significant digits (`1.4G`) and is for reference only. Disk admission, progress, and integrity checks all use
  actual byte counts measured from download responses. When the download finishes, the node writes a manifest of measured sizes as a completion marker; subsequent reconciliation does not download the files again.
- Trailing slashes, query parameters, and fragments are removed from the directory URL, while **hostname case and explicit ports are preserved**. If parsing confirms that you pasted a file URL, it attempts to use the file's containing directory.
- Listing has limits: at most 3 subdirectory levels, 5000 entries in total, 8 MiB per listing page, and a 30-second timeout per listing page. Exceeding a limit returns
  `HTTP_DIRECTORY_TOO_LARGE`—a truncated listing would silently register a model with missing weights, so an error is preferable here.
- A connection failure or an unreadable listing page returns `HTTP_DIRECTORY_UNREACHABLE`.

If downloading on the target node returns `MODEL_SOURCE_REQUIRES_NEWER_AGENT`, upgrade that agent before retrying the download.

## Import an Image Archive from a URL

1. On the Images page, open the **Community** or **Custom** tab, open the **Add Community Image** or **Add Custom Image** page, find **Import from a URL**, enter the `docker save` archive URL, and click **Resolve**.
2. Check the archive size and actual image references. If a reference matches an existing catalog entry, the URL is added to that entry's **Acquisition addresses**, and the page navigates to it. If there is no match, the first resolved reference prefills the new image form; check it and save.
3. If you see a message that **the archive cannot be inspected before it is downloaded** (for example, because the archive is compressed or the file server does not support Range), manually enter the image reference and acquisition address as instructed, then save. The archive contents have not yet been verified; do not infer the tag from the file name.
4. After registration, import the image on the target node as described in the next section. Saving the URL alone does not start a transfer.

To resolve the archive, the server queries its size and reads part of its contents to obtain the actual references, without downloading the entire archive. A known reference mismatch differs from an unavailable preview; see the next section for how node imports handle these cases.

**Acquisition addresses** is an existing image field that now holds both registry references and archive URLs. Archive URLs are only downloaded and loaded;
**they are never pulled as registry references**. The node dropdown, pull candidates, and pull commands all filter them according to this rule. Deployments also do not automatically select archive URLs to download. To use an archive, import it from the node row on the Images page before deploying.

## Node Acquisition Methods: Direct Connection or Management Relay

In the node table on the image details page, the row's **Acquisition Method** includes two URL-related choices in addition to the default **Automatic (Pull Settings)**: **Import from a URL…** (enter an address for this operation),
and addresses already declared for the image, labeled by hostname. For either choice, clicking **Pull** opens a page before downloading starts.

Before starting, choose a download path based on the node's connectivity:

- **Direct connection**: If the node can access the archive, it downloads and loads the archive itself.
- **Management relay**: Use this if the node cannot reach the image source, or only the management node has internet access. The management node downloads the archive and streams it to the node, **without writing it to disk** on the server. This transfer cannot resume. Retrying after an interruption starts from the beginning, and progress shows the bytes transferred.

1. Paste the archive URL (selecting a declared address prefills it and automatically resolves it once).
2. Click **Resolve**. The server uses HEAD + Range to read the archive size and **the actual image references in the archive**.
3. Check whether the references match the current image. If references are resolved but do not include the current image, the page blocks the transfer; replace the URL with the correct archive address. If a preview is unavailable, a warning appears; verify the source and contents before continuing.
4. When using a relay, select **Relay through the management server**.
5. Click **Pull** in the page to start the download.

In strict mode, select the current host for the allowlist before **Resolve**, or before **Pull** with **Relay through the management server**. The host permission is saved before the operation; starting a relay does not require a successful preview first. Automatic previews of a prefilled address do not write permission, and direct node pulls do not grant management-node access.

After you confirm in the page, the address is recorded in the image's **Acquisition addresses**, so it can be selected directly on the image pages for other nodes. This also lets a retry after
an unsuccessful pull find the download source.

If the archive's references cannot be read (because it is compressed or the image source does not support `Range`), the transfer is not blocked; only a warning appears. Such archives cannot be checked before downloading.
After loading the archive, the node records its actual tags in the row's message.

For a direct import, if a tag in the archive differs from the catalog reference, the node adds the catalog reference as a tag. The actual tag is recorded in the row's message for later verification.

After import, the image is `present` on the node and is used directly during deployment. Instances always start with the canonical reference in the catalog.

## Common Errors

| Error Code | Meaning and Resolution |
|---|---|
| `HTTP_DIRECTORY_UNREACHABLE` | The directory listing page cannot be read. Check connectivity from the server to the source and outbound authorization for the internal network, and confirm that the source returns a directory listing |
| `HTTP_DIRECTORY_NOT_A_MODEL` | The directory contains no files, or `config.json` cannot be parsed |
| `HTTP_DIRECTORY_TOO_LARGE` | The file count or directory depth exceeds the limit. Point to the model's own directory |
| `MODEL_SOURCE_REQUIRES_NEWER_AGENT` | The target node's agent is too old. Upgrade it first |
| `IMAGE_ARCHIVE_URL_INVALID` | The archive URL is invalid. It must use `http`/`https` without embedded credentials |
| `IMAGE_ARCHIVE_URL_POLICY_DENIED` | This host is not allowed yet. Ordinary private addresses need no allow list by default, and the allow checkbox is absent while “Restrict private host access” is off. When it is on, check “Automatically add this host to the allow list” beside the address; resolving the address or starting a relay saves the selected host before continuing. Leaving it unchecked never writes permission. Settings → Security & access → Outbound access policy still edits the whole list |
| `IMAGE_ARCHIVE_URL_POLICY_BLOCKED` | This address cannot be permitted through the allow list, for example a reserved or link-local address |
| `IMAGE_ARCHIVE_URL_UPSTREAM_STATUS` | The archive server returned a specific HTTP status, such as 404. Check the path; this is not a network failure |
| `IMAGE_ARCHIVE_URL_UNREACHABLE` | The server did not connect to the archive. Check the address, network, and TLS. A direct node fetch can also fail when the node cannot reach the source |
| `IMAGE_ARCHIVE_URL_NOT_DECLARED` | The URL is not in this image's acquisition addresses. Add it before syncing |

## Related Information

- Terminology: “Model Sources and Downloads” and “Images” in [`CONTEXT.md`](../../CONTEXT.md)
- Other acquisition methods and cache semantics: [Download, Cache, and Sync Models Between Nodes](download-and-sync-models.md)
