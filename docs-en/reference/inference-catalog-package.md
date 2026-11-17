# Executable Inference Catalog Packages

Inference catalog packages distribute **model deployment configurations and accompanying lightweight files**. A package can contain multiple models, and each model can provide multiple specs (profiles) for different deployment configurations. After importing a package, you can view and select these configurations on the **Inference Templates** page.

“Executable” means the package describes how to launch a model: which GPUs, engine, and image to use, where to acquire weights, and which launch parameters to apply. Images, model weights, and runtimes are delivered from their respective sources.

This document explains package structure, import and overwrite rules, auxiliary file delivery, and maintenance of model estimation data. For details on writing individual fields, see the [Inference Catalog model.yaml Syntax Reference](inference-catalog-spec.md).

## Package Contents

The basic file layout is:

```text
manifest.yaml
models/<model-id>/model.yaml
models/<model-id>/assets/<small-file>
```

| File | Purpose |
|---|---|
| `manifest.yaml` | Records the package format version, release information, and model and spec counts |
| `models/<model-id>/model.yaml` | Stores the model's metadata and spec list |
| `assets/` under the model directory | Carries lightweight auxiliary files required by the model |

**Assets are only for lightweight files.** Images, weights, framework libraries, venvs, and shared runtimes must not be placed in assets; they must be delivered through images, weight sources, or node assets.

Catalog packages are identified by a content digest. The server validates path safety and the schema (file structure and field constraints) within the package and computes its content digest. Packages with the same digest can be reused. If the cache is corrupted, it must no longer be served under the old digest.

The `schema_version` in `manifest.yaml` is the package format version, currently 3. A server reads versions up to its own and refuses newer ones; when it sees a newer version on its channel it marks the catalog update as incompatible and does not download it. From 2 on, specs carry `display_name_zh_cn` / `display_name_en`; 3 marks the catalogs that carry `gpus.arch_id` and `tokenizer_ref`, which a server that knows only 2 cannot read because it parses `model.yaml` strictly, field by field. A server that knows 3 reads an official catalog package tolerantly (see "What an Official Package Carries That This Server Cannot Read" below), so a new field in `model.yaml` no longer raises the version; it is raised only when the file layout or the manifest of the package changes.
The database stores the digest and structured records extracted from the package; file contents are stored in the package repository.

### Relationship Between model.yaml and Specs

`model.yaml` uses a flat spec structure: model metadata is declared at the model level, and each deployable configuration is placed in a spec. Each spec describes the following:

| Configuration | Question Answered |
|---|---|
| GPU matching conditions (predicates) | Which GPUs can use this configuration |
| `engine`, `image`, `weights` | Which engine, image, and weight source to use |
| `args`, `env` | Which launch arguments and environment variables to use |
| `pre_start_argv` or `pre_start_script` | What to run before the engine starts |
| API contract | Which interfaces the launched service provides and how to call them |

Specs do not specify `tp`/`nodes`; parallelism is derived from the GPUs actually allocated during deployment.
Choose either `pre_start_argv` or `pre_start_script`. The script size limit is 64 KiB.

See [ADR 0009](../adr/0009-flat-spec-catalog.md) and [ADR 0010](../adr/0010-executable-inference-catalog-package.md) for the design rationale behind the flat structure and multi-file packages, respectively. See the [API Reference](api.md) for import, export, and sync interfaces.

## Importing and Overwriting

### Import Entry Points and File Formats

The Web UI provides two types of entry points:

- Custom templates: **Inference Templates → Import Templates**, which calls `POST /api/v1/inference-templates/import`.
- Official catalog packages: the upload entry point under **Settings → Version and updates**, which calls `POST /api/v1/inference-templates/official`.

The custom template file picker accepts `.yaml`, `.yml`, `.json`, `.tar.gz`, and `.tgz`. When calling the import API, send the **raw file bytes** directly as the request body, without multipart. Add `?overwrite=true` to overwrite existing templates.

The server selects the parser based on the gzip format signature (magic) at the start of the input:

| Input | Parsing Method and File Restrictions |
|---|---|
| gzip data | Parsed as a catalog package; can include assets and a README |
| Non-gzip data | Parsed as bare catalog YAML; cannot upload asset or README file contents this way |

If bare YAML references local files and `overwrite=true` is not specified, the server returns `CATALOG_PACKAGE_REQUIRED_FOR_FILES`.
With `overwrite=true`, you can modify the structured content of an existing packaged template and reuse its files: assets must come from the corresponding user spec's current package, and the README must already exist. The server repackages the content and verifies asset SHAs. Adding new files still requires uploading a catalog package.

The size limits are:

- Import API request body: up to **32 MiB**.
- gzip catalog package: up to **16 MiB** compressed and **64 MiB** total file size after extraction.

A successful response is `{models, package_digest}`, containing the number of imported models and the package's content digest, respectively.

### tar Package Path and File Requirements

The following issues cause import to return HTTP 400:

| Package Issue | Error Message |
|---|---|
| An entry name is not a normalized relative path, such as one with a redundant `./` prefix | `unsafe inference catalog package path "./manifest.yaml"` |
| Non-regular file entries, such as symbolic links | `is not a regular file` |
| macOS AppleDouble files, such as `._x` | `contains unexpected file` |

Directory entries are skipped after path validation. Do not package with `tar -C pkg .`, because it creates entries with a `./` prefix.

Run the following command in the package directory containing `manifest.yaml`. Use `-T` to list files explicitly, and `COPYFILE_DISABLE=1` to prevent macOS from writing AppleDouble files. Place the output in the parent directory to avoid including an old archive when packaging again.

```bash
COPYFILE_DISABLE=1 tar -czf ../pkg.tgz --no-recursion \
  -T <(find . -type f ! -name '._*' | sed 's|^\./||' | sort)
```

zsh does not perform word splitting on unquoted variables. `tar ... $FILES` treats a multi-line file list as one argument, so use the `-T` file-list method above.

### What an Overwrite Import Preserves

**An overwrite import only adds or updates; it never implicitly deletes.** The server adds or updates models/specs by name:

- Existing records with a user source are not deleted when absent from the new package.
- Old specs missing from the new package remain visible, selectable, and deployable.

To retire a user spec, you must explicitly delete it using the delete button on the template details page or by calling:

```text
DELETE /api/v1/inference-templates/spec?model_id=…&spec=…
```

Deletion is rejected while any deployment still references the spec. Therefore, after removing a spec from a package, the publisher must also arrange this explicit deletion step.

### What an Official Package Carries That This Server Cannot Read

The official catalog is written for the newest server. On "check for updates" or an official package upload, the parts an older server cannot read are skipped and everything else is applied as usual (see [ADR 0065](../adr/0065-official-catalog-read-tolerantly.md) for the rationale):

- A spec that cannot be parsed or validated: that spec is skipped.
- A model whose own fields cannot be read: the whole model is skipped, and so are the specs that reference it as a component.
- A skipped model or spec that was imported before stays as it is and can still be deployed; a new one is not shown on this server yet.
- `skipped` in the response lists the `model_id`, `spec` (omitted when the whole model is skipped) and `reason` of each, and the settings page keeps the list on screen. After a server upgrade, updating once more reads everything.

The checks on the package itself are not relaxed: any mismatch in the file layout, the model and spec counts in the manifest, or the asset digests of the specs that are kept refuses the whole package. This tolerance applies to official catalog packages only; through "Import templates" an unknown field is still an error.

## Delivering and Using Auxiliary Files

### Asset Delivery Path

The package contains the actual asset bytes and is stored by content digest in the package repository. During deployment, the platform delivers them as follows:

1. The server writes the asset content and sha256 checksum into `RuntimeAsset` and sends it with the instance's `DesiredState` (desired state).
2. The agent verifies the assets and unpacks them into a read-only cache.
3. The agent mounts the cached content at the location specified by `unpack_to`.

Nodes do not need access to external addresses to acquire these packaged assets.

**Prefer packaging auxiliary files into a single `.tar.gz` and pointing `unpack_to` to a directory.** This ensures compatibility with different container launchers:

- The old udocker CLI launcher only supports directory mounts. Single-file bind mounts silently fail, resulting in “deployment succeeds, but the file is missing.”
- runveil is the default launcher for the `udocker` driver and uses PRoot bind mounts directly. Single-file assets have not yet been separately verified, so continue to distribute them as directories.

### Plugin Arguments Require a File Source

In `args`, `--*-plugin` arguments such as `--reasoning-parser-plugin` and `--tool-call-parser-plugin` must point to plugin files. Static validation handles them according to the path form:

- **Absolute paths**: accepted, but this does not prove that the file actually exists.
- **Relative paths**: resolved relative to the container working directory `/`. There must be evidence of a source in the spec's assets, component mounts, pre-start commands/scripts, or entry command.

For example, a spec once declared `--reasoning-parser-plugin nano_v3_reasoning_parser.py`, but neither the image, the weight repository, nor assets supplied the file, causing a `FileNotFoundError` when the engine started.
For such arguments whose file source cannot be found within the spec, validation returns the stable error code `CATALOG_FILE_VALUED_ARG_UNRESOLVED`.

This check only catches statically detectable cases where “the file has no declared source.” The control plane cannot read image contents, so L2/L3 probes are still needed to confirm that the file actually exists. A filename appearing in a script also does not verify that the script correctly generates the file.

## model_config: Model-Level Estimation Facts

`model_config` is declared at the **model level** and shared by all specs for the same model. It stores fields relevant to deployment estimation from the model's `config.json`, keeping the original field names. Example field syntax:

```yaml
model_config:
  model_type: qwen3
  num_hidden_layers: 64
  hidden_size: 5120
  num_attention_heads: 64
  num_key_value_heads: 8
  head_dim: 128
  max_position_embeddings: 40960
```

### Why the Catalog Includes This Data

Deploying from a catalog template does not register a model, so there is no local `config.json` to read. Fetching it from the weight repository at deployment time does not cover every case either:

- Gated repositories require credentials, so the server cannot read them directly.
- Some repositories' `config.json` files lack complete attention structure information, as with Gemma 3.
- Diffusion models may not have a `config.json`.

The catalog therefore supplies these model facts in the package. ModelScope and HuggingFace use the same `config.json` layout, so `model_config` does not depend on the selected weight source.

### Difference Between model_config and KV Capacity

| Information | Purpose |
|---|---|
| `model_config` | Provides the attention structure, context length, and information required for encoder estimation, including whether the relevant dimensions are divisible by the tensor parallelism degree |
| `kv_bytes_per_token` | Stores the measured KV cache bytes per token for each engine to estimate the KV pool's token capacity; if the value for the corresponding engine is missing, the deployment form displays “No token capacity” |
| `model_config.kv_cache: false` | Explicitly declares that the model has no token-indexed KV cache, as with diffusion or pure encoder models; KV capacity estimation is then “Not applicable” |

`kv_cache: false` is a catalog extension declaration, not an original `config.json` field. “Not applicable” means the model is known not to require a KV cache; it must not be confused with missing configuration or measured data. See [Model-Level Fields](inference-catalog-spec.md#模型级字段) for complete field constraints.

### Maintaining and Validating These Facts

The maintenance tool is `cmd/gpuworks-catalog-model-config`:

- Read the actual `config.json` from the weight sources already declared in the spec and parse the model structure. By default, the tool only reports changes; use `-write` to write them back. Gated repositories require `HF_TOKEN`.
- Use `-verify` to additionally check layer counts and attention types against the published weight index, including the distinction between MLA latent representations and explicit K/V.
- For the few repositories whose structure information cannot be read, record the mirror repository and subdirectory used, or the structure information declared field by field and its supporting evidence, in `fallback-sources.json`.

Tests under `internal/server/inferencecatalog/seed` check whether `model_config` retains the same information after storage and whether measured KV values are reasonable, and track measurement coverage. Models that have not been measured are recorded as coverage gaps. Current tests do not fail for incomplete coverage, so passing tests do not mean all models have KV capacity data.

When checking weight indexes, also account for these differences:

- The DeepSeek-V3 family may have one more MTP layer than the configuration specifies (61+1); vision and audio encoders may contain multiple parallel stacks. `-verify` already classifies these cases.
- The `dots-studio/dots3-note-prev` checkpoint has one more layer than its config. The serving engine constructs the model from the config, so the config takes precedence.

<!-- translation-source-sha256: 29039837d5399a351989a6b01249271b1061df159581b1aeb6681bb2e7431a79 -->
