package inferencecatalog

import (
	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// JSONSchema returns the JSON Schema (Draft 2020-12) of the flat catalog
// YAML document that /inference-templates/import accepts and the editor
// serves. It is the server-owned dictionary for editor autocomplete, hover
// documentation, and first-pass validation; the authoritative validation
// remains ParseYAML + ValidateFlatCatalog (rules a schema cannot express,
// such as cross-model component references, only run there).
//
// Enumerations are built from the code-owned vocabularies (engines.Names,
// inferenceapi.Definitions) so the schema cannot drift into a second source
// of truth; schema_test.go additionally reflects over the YAML struct tags
// to reject silent field additions.
func JSONSchema() map[string]any {
	operationTypes := make([]any, 0, 16)
	for _, definition := range inferenceapi.Definitions() {
		operationTypes = append(operationTypes, definition.Type)
	}
	engineNames := make([]any, 0, 4)
	for _, name := range engines.Names() {
		engineNames = append(engineNames, name)
	}
	schema := map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"$id":                  "https://gpuworks/schemas/inference-catalog-flat.json",
		"title":                "gpuworks inference catalog (flat YAML)",
		"description":          "Import document for inference templates. Exactly one YAML document; unknown keys are rejected by the server, so the editor treats them as errors too.",
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"models"},
		"properties": map[string]any{
			"models": map[string]any{
				"type":        "array",
				"minItems":    1,
				"description": "Catalog models; each entry is one model identity with its deployable specs.",
				"items":       modelSchema(operationTypes, engineNames),
			},
		},
	}
	return schema
}

func modelSchema(operationTypes, engineNames []any) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"model_id", "specs"},
		"properties": map[string]any{
			"model_id": map[string]any{
				"type":        "string",
				"description": "Unique org/repo identity, lowercased on save. Also the package directory name.",
				"pattern":     "^[^\\s/]+(?:/[^\\s/]+)*$",
				"minLength":   1,
			},
			"source": map[string]any{
				"type":        "string",
				"enum":        []any{"upstream", "user"},
				"description": "Provenance tag; user imports are stored as user regardless.",
			},
			"display_title": map[string]any{
				"type":        "string",
				"description": "Human-facing title shown in the catalog.",
			},
			"parameter_count": map[string]any{
				"type":        "string",
				"description": "Free text, e.g. \"8B\" or \"235B-A22B\".",
			},
			"architecture": map[string]any{
				"type":        "string",
				"description": "Free text architecture family for display.",
			},
			"input_modalities": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string", "enum": []any{"text", "image", "audio", "video"}},
				"description": "Deduplicated, order-preserving input modality list.",
			},
			"model_config": map[string]any{
				"type":                 "object",
				"description":          "config.json estimation subset used by the VRAM estimator; usually filled by gpuworks-catalog-model-config, not by hand.",
				"additionalProperties": true,
			},
			"readme_zh_cn": map[string]any{
				"type":        "string",
				"enum":        []any{"README.zh-CN.md"},
				"description": "Declares the package carries this localized guide.",
			},
			"readme_en": map[string]any{
				"type":        "string",
				"enum":        []any{"README.en.md"},
				"description": "Declares the package carries the English guide.",
			},
			"kv_bytes_per_token": map[string]any{
				"type":                 "object",
				"description":          "Measured KV cache bytes per token per engine; a missing engine means \"no figure\" rather than zero.",
				"propertyNames":        map[string]any{"enum": engineNames},
				"additionalProperties": map[string]any{"type": "integer", "minimum": 0},
			},
			"context_length": map[string]any{
				"type":        "integer",
				"minimum":     0,
				"description": "Declared context length in tokens.",
			},
			"release_date": map[string]any{
				"type":        "string",
				"pattern":     "^[0-9]{4}-[0-9]{2}-[0-9]{2}$",
				"description": "Release date, YYYY-MM-DD and a real calendar date.",
			},
			"unsupported": map[string]any{
				"type":        "array",
				"description": "Model-level \"not usable here\" declarations surfaced with their reason.",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"gpus", "reason"},
					"properties": map[string]any{
						"gpus":         gpuSelectorSchema(),
						"quantization": map[string]any{"type": "string", "description": "Limits the declaration to one quantization."},
						"reason":       map[string]any{"type": "string", "minLength": 1, "description": "Shown to the user when a matching chip hits this entry."},
					},
				},
			},
			"specs": map[string]any{
				"type":        "array",
				"minItems":    1,
				"description": "Deployable configurations; names unique per model and per source.",
				"items":       specSchema(operationTypes, engineNames),
			},
		},
	}
}

func specSchema(operationTypes, engineNames []any) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"name", "engine", "image", "gpus"},
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "Spec identity within the model; same name + user source = replacement on import.",
			},
			"display_name_zh_cn": map[string]any{
				"type":        "string",
				"maxLength":   maxDisplayNameZHCN,
				"description": "Chinese display name: cards · quantization · what tells the spec from its siblings. Unique within the model and engine.",
			},
			"display_name_en": map[string]any{
				"type":        "string",
				"maxLength":   maxDisplayNameEN,
				"description": "English display name, same rules as display_name_zh_cn.",
			},
			"display_profile": map[string]any{
				"type":        "string",
				"deprecated":  true,
				"description": "Retired; ignored on import. Use display_name_zh_cn and display_name_en.",
			},
			"scenario": map[string]any{
				"type":        "string",
				"enum":        []any{"latency", "balanced", "throughput"},
				"description": "Workload scenario the tuned defaults target.",
			},
			"engine": map[string]any{
				"type":        "string",
				"enum":        engineNames,
				"description": "Serving engine; parameters come from the server /engines dictionary.",
			},
			"image": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "Container image reference (digest-pinned recommended).",
			},
			"image_sources": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":      "string",
					"minLength": 1,
					"maxLength": maxImageRefLength,
					// Surrounding whitespace is trimmed on import; whitespace
					// inside a reference is refused (normalizeImageSources).
					"pattern": "^\\s*\\S+\\s*$",
					"not":     map[string]any{"pattern": "://"},
				},
				"uniqueItems": true,
				"description": "Further registry references the image can be pulled from, preferred first; the deployer picks one on the deploy page and image itself is tried after them. Not checked against image: a source may be a tag of a digest-pinned image. Entries are compared as the registry resolves them (nginx:1 and docker.io/library/nginx:1 are the same; host and repository ignore case, tags do not); an entry equal to image is dropped.",
			},
			"weights": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"sources"},
				"properties": map[string]any{
					"quantization": map[string]any{"type": "string", "description": "e.g. bf16, fp8, int8, w8a8, gptq."},
					"sources":      weightSourcesSchema(),
				},
			},
			"weight_bytes": map[string]any{
				"type":        "integer",
				"minimum":     0,
				"description": "Total weight artifact size in bytes; backfilled from the first source when omitted.",
			},
			"runtime_weight_bytes": map[string]any{
				"type":        "integer",
				"minimum":     0,
				"description": "On-disk runtime footprint when it differs from weight_bytes.",
			},
			"gpus": gpuSelectorSchema(),
			"args": stringArraySchema("Engine argv tokens; ${VARIABLES} expand from placement/runtime."),
			"env":  envSchema(),
			"pd_mode": map[string]any{
				"type":        "string",
				"enum":        pdmode.Names(),
				"description": "This spec is deployed only as PD disaggregation in this mode; its args and env are what the model needs there.",
			},
			"pd_roles": map[string]any{
				"type":        "object",
				"description": "The layout a PD-only spec is written for, by role (needs pd_mode): one instance's TP and PP, the nodes it spans and how many instances a deployment starts with. The deploy form starts from it.",
				"additionalProperties": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"tp"},
					"properties": map[string]any{
						"tp":    map[string]any{"type": "integer", "minimum": 1},
						"pp":    map[string]any{"type": "integer", "minimum": 0, "description": "Pipeline degree; 0 or absent means 1."},
						"nodes": map[string]any{"type": "integer", "minimum": 0, "description": "Nodes one instance spans; TP x PP must divide evenly over them. 0 or absent means 1."},
						"units": map[string]any{"type": "integer", "minimum": 0, "maximum": pdmode.MaxUnitsPerRole, "description": "Instances a deployment starts with; 0 or absent means 1."},
					},
				},
			},
			"manual_command": map[string]any{
				"type":        "string",
				"maxLength":   MaxManualCommandBytes,
				"description": "A shell script the deployment runs as its manual command, with ${GPUWORKS_*} launch variables expanded per rank. A PD spec's script must reference ${GPUWORKS_PD_ROLE}; one with a multi-node role also ${GPUWORKS_NODE_RANK} or ${GPUWORKS_HEADLESS}.",
			},
			"shm_bytes": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"maximum":     MaxSpecShmBytes,
				"description": "The /dev/shm size in bytes the engine needs (for example an engine that keeps large lookup tables in shared memory). The container gets at least this much; a larger engine default plus the CPU KV tier still applies, and the deployment's own shm_bytes overrides it.",
			},
			"start_timeout_sec": map[string]any{
				"type":        "integer",
				"minimum":     store.DeploymentStartTimeoutSecLo,
				"maximum":     store.DeploymentStartTimeoutSecHi,
				"description": "Seconds an instance of this spec may take to become healthy, for a recipe with a long first start. It replaces the cluster default; a deployment's own start_timeout_sec overrides it.",
			},
			"memlock_unlimited": map[string]any{
				"type":        "boolean",
				"description": "Start the container with unlimited locked memory (--ulimit memlock=-1:-1), for an engine that pins large host memory (for example with cudaHostRegister). A memlock ulimit in the deployment's docker_args overrides it.",
			},
			"docker_args": stringArraySchema("Extra docker run options the engine needs (for example --cap-add=SYS_PTRACE), applied before the deployment's own docker_args and the platform's. Validated the same way as the deployment's own container_overrides.docker_args: platform-managed options (--name, --network, --gpus, --shm-size, -e, -v, --mount, --entrypoint, ...) are refused, and --ipc=host together with shm_bytes is refused. Options that hand the container the host (--privileged, --pid) are allowed; the deploy preview warns about them."),
			"dirs": map[string]any{
				"type":        "array",
				"maxItems":    deploy.MaxManagedDirs,
				"description": "Writable host directories the platform keeps for the deployment on every node it runs on: created on first start, mounted read-write at mount_path, kept while the deployment's instances are restarted or recreated, and removed when the deployment is deleted. Admission requires each node's managed-directory disk to have the declared limits plus the disk reserve free.",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"name", "mount_path", "limit_bytes"},
					"properties": map[string]any{
						"name":        map[string]any{"type": "string", "pattern": "^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$", "description": "Unique within the spec; names the directory on the node."},
						"mount_path":  map[string]any{"type": "string", "pattern": "^/.+", "description": "Absolute container path; may not overlap the model mount, a component mount_path, an asset unpack_to or another dir."},
						"limit_bytes": map[string]any{"type": "integer", "minimum": 1, "maximum": deploy.MaxManagedDirLimitBytes, "description": "Most the template writes there, in bytes like the spec's other sizes: reserved on every node at admission. The platform does not prune or cap the directory; staying within it is the recipe's job."},
						"retain":      map[string]any{"type": "string", "enum": []string{deploy.ManagedDirRetainDeployment}, "description": "How long the directory lives; deployment (the default) keeps it until the deployment is deleted."},
					},
				},
			},
			"pre_start_argv": stringArraySchema("One command run inside the container, not on the host, every time the container starts, before the engine command; entries must be nonempty. The agent runs it from the wrapper /gpuworks/pre-start.sh (/bin/sh with set -eu, then exec of the engine command), with the container's full environment: the spec's env, the deployment's env and the accelerator variables the agent injects. ${GPUWORKS_*} variables are expanded by the server at deploy time. A nonzero exit stops the start: the engine never runs and the instance fails as an exited container, with the command's output as evidence. Mutually exclusive with pre_start_script."),
			"pre_start_script": map[string]any{
				"type":        "string",
				"description": "Shell script run inside the container, not on the host, every time the container starts, before the engine command; at most 64 KiB. The agent writes it into the wrapper /gpuworks/pre-start.sh, which /bin/sh runs with set -eu and which then execs the engine command, so the script sees the container's full environment (the spec's env, the deployment's env and the accelerator variables the agent injects). ${GPUWORKS_*} variables are expanded by the server at deploy time; other ${NAME} references are left to the shell. Any failing command or unset variable stops the start: the engine never runs and the instance fails as an exited container, with the script's output as evidence. Mutually exclusive with pre_start_argv.",
			},
			"assets": map[string]any{
				"type":        "array",
				"description": "Package-local files shipped with the spec (content-addressed).",
				"items": map[string]any{
					"type": "object",
				},
			},
			"entrypoint": stringArraySchema("Overrides the engine image entrypoint."),
			"placement": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"language", "resolver"},
				"properties": map[string]any{
					"language": map[string]any{"type": "string", "enum": []any{"starlark"}, "description": "Resolver language; starlark is the only supported one."},
					"resolver": map[string]any{"type": "string", "minLength": 1, "description": "Starlark: def resolve(gpu) -> accept(...) or reject(reason)."},
				},
			},
			"api": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"health_endpoint": map[string]any{"type": "string", "description": "HTTP health path, e.g. /health."},
					"tokenizer_ref":   map[string]any{"type": "string", "description": "Verified tokenizer repository, independent of weights: hf://org/repo@<40-hex-commit> or ms://org/repo@<40-hex-commit>. Used by benchmark auto resolution when no node vocabulary is available."},
					"operations": map[string]any{
						"type":        "array",
						"description": "Declared inference operations; endpoints/modes get registry defaults.",
						"items": map[string]any{
							"type":     "object",
							"required": []string{"type"},
							"properties": map[string]any{
								"type":     map[string]any{"type": "string", "enum": operationTypes, "description": "Stable operation vocabulary from the server registry."},
								"endpoint": map[string]any{"type": "string", "description": "Override endpoint path (default comes from the operation registry)."},
								"mode":     map[string]any{"type": "string", "description": "sync | stream | async."},
							},
						},
					},
					"metrics":       apiMetricsSchema(),
					"request_stats": apiRequestStatsSchema(),
				},
			},
			"components": map[string]any{
				"type":        "array",
				"description": "Additional weights mounted into the primary model (e.g. a reranker).",
				"items": map[string]any{
					"type":     "object",
					"required": []string{"name", "role", "weight_bytes", "mount_path"},
					"properties": map[string]any{
						"name":     map[string]any{"type": "string"},
						"role":     map[string]any{"type": "string"},
						"model_id": map[string]any{"type": "string", "description": "Referenced catalog model; must exist."},
						"weights": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"quantization": map[string]any{"type": "string"},
								"sources":      weightSourcesSchema(),
							},
						},
						"weight_bytes": map[string]any{"type": "integer"},
						"mount_path":   map[string]any{"type": "string"},
					},
				},
			},
			"performance_tradeoffs": performanceTradeoffsSchema(),
		},
	}
}

// apiMetricsSchema is api.metrics (inferenceapi.Metrics). Metric names and the
// engines a template may inherit come from internal/inferencemetrics.
func apiMetricsSchema() map[string]any {
	metricNames := make([]any, 0, 40)
	for _, name := range inferencemetrics.Names() {
		metricNames = append(metricNames, name)
	}
	inheritEngines := make([]any, 0, 2)
	for _, set := range inferencemetrics.DetectSets() {
		inheritEngines = append(inheritEngines, set.Prefix)
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"description":          "The engine's metrics endpoint and how its metrics map onto platform inference metrics. Omit for vllm/sglang; an undeclared callable is matched against the vllm/sglang mappings by family prefix.",
		"properties": map[string]any{
			"endpoint": map[string]any{"type": "string", "description": "Metrics path on the metrics port; default /metrics."},
			"port": map[string]any{
				"type": "string", "enum": []any{"api", inferenceapi.MetricsPortSeparate},
				"description": "api (default): the endpoint is on the API port. separate: the platform reserves a second port per instance, states it as ${GPUWORKS_METRICS_PORT}, and the agent scrapes that port; the launch must reference the variable.",
			},
			"format": map[string]any{
				"type": "string", "enum": []any{string(inferencemetrics.FormatPrometheus), string(inferencemetrics.FormatJSON)},
				"description": "prometheus (default): map keys are exposition families. json: the endpoint answers one JSON document and map keys are dotted paths of numbers in it, like totals.output_tokens, or a string compared as path==word / path!=word (1 or 0, gauges only); no inherit, counters and gauges only.",
			},
			"inherit": map[string]any{"type": "string", "enum": inheritEngines, "description": "Start from this engine's built-in mapping."},
			"map": map[string]any{
				"type":        "object",
				"description": "Exposition family (optionally name{label=\"value\"}) to platform metric; entries win over inherited ones.",
				"additionalProperties": map[string]any{
					"oneOf": []any{
						map[string]any{"type": "string", "enum": metricNames},
						map[string]any{
							"type":                 "object",
							"additionalProperties": false,
							"required":             []string{"to"},
							"properties": map[string]any{
								"to":    map[string]any{"type": "string", "enum": metricNames},
								"scale": map[string]any{"type": "number", "exclusiveMinimum": 0, "description": "Multiplies a counter or gauge; histograms cannot be scaled."},
							},
						},
					},
				},
			},
		},
	}
}

// apiRequestStatsSchema is api.request_stats (inferenceapi.Contract.RequestStats):
// reply fields to the request stats of inferencemetrics.RequestStats.
func apiRequestStatsSchema() map[string]any {
	stats := make([]any, 0, 3)
	for _, stat := range inferencemetrics.RequestStats() {
		stats = append(stats, stat.Name)
	}
	return map[string]any{
		"type":        "object",
		"description": "Fields the engine writes into each reply (the last event of a stream, or a plain reply's body) mapped onto per-request stats, keyed by dotted path from the event's top-level object, like tensorfold.prefill_s. Omit for vllm, whose per-request metrics are known.",
		"additionalProperties": map[string]any{
			"oneOf": []any{
				map[string]any{"type": "string", "enum": stats},
				map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"to"},
					"properties": map[string]any{
						"to":    map[string]any{"type": "string", "enum": stats},
						"scale": map[string]any{"type": "number", "exclusiveMinimum": 0, "description": "Multiplies the value: 0.001 turns milliseconds into seconds."},
					},
				},
			},
		},
	}
}

func gpuSelectorSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"vendor"},
		"description":          "GPU predicate: vendor plus optional compute-capability expression, case-insensitive name regex, and exact architecture ID.",
		"properties": map[string]any{
			"vendor": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "nvidia | amd | ascend | hygon | ppu | apple (lowercased on save).",
			},
			"cc": map[string]any{
				"type":        "string",
				"pattern":     "^(?:[0-9]+(?:\\.[0-9]+)?|(?:>=|<)[0-9]+(?:\\.[0-9]+)?|[0-9]+(?:\\.[0-9]+)?\\.\\.[0-9]+(?:\\.[0-9]+)?)$",
				"description": "Compute capability: exact value, >=x, <x, or a..b range.",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Case-insensitive regular expression matched against GPU names.",
			},
			"arch_id": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "Nonblank exact architecture ID stored for the GPU; compared case-insensitively. A missing GPU arch_id does not match this selector.",
			},
			"display_target": map[string]any{
				"type":        "string",
				"deprecated":  true,
				"description": "Retired; ignored on import. The spec's display names say which cards it is for.",
			},
		},
	}
}

func weightSourcesSchema() map[string]any {
	repository := map[string]any{"type": "string", "minLength": 1, "description": "Organization/repository identity."}
	local := map[string]any{
		"type": "string", "minLength": 1,
		"description": "Registered local model identity, not a host path or URL.",
		"not":         map[string]any{"pattern": `(^/|/$|//|^[A-Za-z]:/|(^|/)\.{1,2}(/|$)|\\)`},
	}
	// The compact scalar and expanded object are both accepted by ParseYAML.
	// State each provider explicitly; overlapping allOf/additionalProperties
	// definitions are merged differently by the editor's schema validator.
	source := func(identity map[string]any) map[string]any {
		return map[string]any{"anyOf": []any{
			identity,
			map[string]any{
				"type": "object", "additionalProperties": false, "required": []string{"repo_id"},
				"properties": map[string]any{
					"repo_id":   identity,
					"revision":  map[string]any{"type": "string", "description": "Repository commit or registered local revision, when known."},
					"subfolder": map[string]any{"type": "string", "description": "Repository subpath when weights are nested: a directory, or a single file when no directory has that name. In the spec's own weights or a component's it may name the rank, as in rank${GPUWORKS_NODE_RANK} (the older ${NODE_RANK} is an alias): each rank of a multi-node deployment then downloads and mounts only its own subfolder, and the block's weight_bytes is one rank's size."},
				},
			},
		}}
	}
	return map[string]any{
		"type": "array", "minItems": 1,
		"description": "Weight sources: downloadable repositories or registered local model identities.",
		"items": map[string]any{
			"type": "object", "minProperties": 1, "maxProperties": 1,
			"propertyNames":        map[string]any{"enum": []any{"huggingface", "modelscope", "local"}},
			"additionalProperties": false,
			"properties": map[string]any{
				"huggingface": source(repository), "modelscope": source(repository), "local": source(local),
			},
		},
	}
}

func stringArraySchema(description string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": description}
}

func envSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": map[string]any{"type": "string"},
		"description":          "Environment variables injected into the container.",
	}
}
