# Save a Deployment as an Inference Template

**When to use this**: you have a model running in GPUWorks (the service is up and its API answers) and want to turn that configuration into an inference template, to reuse it yourself or on another cluster, or to contribute it to the official catalog, without writing `model.yaml` by hand.

When you are done you have a flat YAML document that imports as it is, and a test record. Every fact in the YAML comes from what the deployment actually ran; nothing has to be filled in by hand.

**Prerequisites**: the deployment is running and has at least one running instance; you can sign in to the console (exporting does not require an administrator, saving into the catalog does).

## Where the data comes from

| Template field | Source |
|---|---|
| `image` | The image the instance actually runs |
| `args` | The deployment's launch arguments: form fields, template arguments and user arguments together, written the way the engine actually builds its command line; arguments the platform injects itself (model path, port, served name, TP/PP, distributed arguments) are not written |
| `env` | The source template's environment variables; variables the deployment added itself are not exported by default, see below |
| `gpus` | The vendor and model of the instance's cards, the model written as a pattern that matches only that model (such as `^NVIDIA H20$`) |
| `weights` | A deployment made from a template keeps the template's weight sources; a custom deployment gets the commit the copy its instance reads resolved to |
| `model_config`, `context_length`, `input_modalities` | Current catalog model metadata for a template deployment (estimation facts that may change with catalog updates), or the registered model row's config.json and metadata for a custom deployment; these are not claimed to be a launch-time snapshot |
| `api` | The API declaration in the instance's immutable launch record, used to choose operation checks |
| `kv_bytes_per_token` | Written only when there is a reliable source: the source template, or the catalog's value for the same model. Otherwise it is left out and a comment says so |
| `placement` | A deployment made from a template keeps the template's; a custom deployment states none, so estimation and scheduling decide the card count, and the tested card count and memory per card go into a comment |

One success proves one card model, so `gpus` matches only the tested model; widening it to more models is an explicit edit by you or the reviewer. A dGPU memory limit is this deployment's resource choice, not a requirement of the model, so it only goes into a comment.

Export uses one fixed instance and its immutable launch record. Saved configuration that has not been applied is not treated as tested. Changed inputs, a changed instance during checks, or a gateway response from another replica cannot verify this export; apply the configuration and retry. Later catalog updates do not replace the executed weights, pre-start script, topology or API declaration.

## In the console

On the Deployments page, find the running deployment, open the More menu at the end of its row and choose Save as template. The dialog tests the API first, then shows:

- **API checks**: the result for each operation; an operation that was not tested says why.
- **What the template states and what is missing**: the same facts as the comments at the top of the YAML, such as the card it was verified on, the placement left out, or the missing KV cost per token.
- **Environment variables left out**: tick the ones that are really needed and hold no secret, then choose Rebuild.
- **Template YAML**: you can edit it in place.

Anyone can use Download YAML and Copy test record at the bottom; Save to catalog is shown only to administrators and is unavailable when a check failed. After saving, the dialog links to the template. The steps below are the same flow through the API.

## 1. Export

```bash
curl --silent --show-error -X POST \
  -H "Authorization: Bearer $TOKEN" \
  http://<server>/api/v1/deployments/<deployment-id>/catalog-template
```

Before writing the template, the server sends the smallest request to each chat, completions and embeddings operation the deployment declares (the same request the health check sends; it is not metered). Video, image, speech and other operations that are slow or costly to test get no request; a comment marks them as not tested, and the export still succeeds.

The returned `yaml` starts with comments, for example:

```yaml
# Saved from GPUWorks deployment "olmo" (id 12).
# Verified on 1 x NVIDIA H20.
# No placement: the deployment ran on 1 card(s) with 97871 MiB each.
# Missing kv_bytes_per_token for vllm: compute it from config.json
# (scripts/estimate-calib/kv_layout.py) before proposing this template.
# Deployment env not exported: HF_TOKEN.
# Tested openai.chat: HTTP 200.
models:
  - model_id: allenai/olmo-2-0425-1b-instruct
    ...
```

The comments do not change what the document parses to. Fields and error codes are in the [API reference: Save as an inference template](../reference/api.md#另存为推理模板).

## 2. Decide which environment variables to keep

Variables the deployment added itself often hold tokens (such as `HF_TOKEN`) and host paths, so none are exported by default; their names are listed in `excluded_env`. Once you have confirmed a variable is something the model needs and holds no secret, name it in the request body:

```bash
curl --silent --show-error -X POST \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"include_env": ["VLLM_USE_V1"]}' \
  http://<server>/api/v1/deployments/<deployment-id>/catalog-template
```

Container overrides such as mounts and docker arguments describe one host and are not exported.

## 3. Save into the catalog or propose it upstream

- **Save into this cluster's catalog**: an administrator checks the `yaml` with `POST /api/v1/inference-templates/evaluate`, then imports it with `POST /api/v1/inference-templates/import`. An export of a deployment made from a template is a new user spec under the original model, and importing it leaves the existing specs alone. An export with a failed check (`saveable: false`) is for troubleshooting only; do not import it.
- **Contribute to the official catalog**: submit the `yaml` together with `evidence` (a plain-text record of the GPU, driver, image, engine and test results) in an issue or merge request. The reviewer fills in what the comments list as missing (such as `kv_bytes_per_token`).

## When the export is refused

For 409 `TEMPLATE_EXPORT_UNAVAILABLE`, read `params.reason`:

| reason | Cause and what to do |
|---|---|
| `not_running` | The deployment is not running or has no running instance; start it first |
| `distributed`, `pd` | Multi-node and PD deployments cannot be exported yet |
| `template_assets`, `components` | The source template carries packaged files or component models, which one YAML document cannot hold; use the template editor instead |
| `weights_not_public` | The weights come from a local directory or a URL that nobody else can download |
| `revision_unpinned`, `revisions_differ` | The weight revision is a branch name, or the copies resolved different commits; pin the model to one commit and deploy again |
| `launch_changed` | Configuration or the running instance changed; apply it and restart before retrying |
| `launch_snapshot_missing` | The instance has no complete, verifiable launch snapshot; restart it with the current server version |
| `heterogeneous_gpus` | The instance uses different card models that one tested-model selector cannot describe |
| `command_overridden` | The deployment replaced the engine command, so its launch cannot be written as template arguments |
| `no_gpu_facts`, `template_missing`, `model_missing` | The instance is not bound to specific cards, or the source template or model has been deleted |

409 `LAUNCH_NOT_EXPORTABLE` means the exported arguments cannot rebuild the deployment's original command line, and the platform refuses to give an approximation; report the deployment's launch arguments and engine version to the maintainers.
