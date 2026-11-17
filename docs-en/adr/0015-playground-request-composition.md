# ADR 0015: Compose Playground Requests From Protocol Definitions × Catalog Declarations × Engine Discovery × User Overrides, With an Expert Mode for Direct Raw Request Editing

Date: 2026-09-05 · Status: Accepted

## Context

Deploying `Qwen/Qwen3-TTS-12Hz-0.6B-Base` on gpu3 reveals two types of problems:

1. “Open Playground” in the deployment list hardcodes `/playground/chat?model=<served>` for every deployment (`ui/src/pages/DeploymentsPage.tsx`), ignoring the deployment's declared API contract. When the chat page cannot find that model, it silently falls back to the first chat model and rewrites the URL to another model. The inference template details page correctly selects a route based on the operation's `renderer`. Two implementations handle the same action.
2. The speech synthesis page hardcodes the request body as `{input, voice:"alloy", response_format:"wav"}`; the same hardcoding also exists in the example code generator (`inferenceapi/examples.go`) and L3 template validation (`templateprobe/evaluation_audio.go`). For Qwen3-TTS, vllm-omni semantics are as follows: Base weights have no preset speakers and support only voice cloning, requiring `task_type=Base` plus `ref_audio`/`ref_text`; CustomVoice speakers include vivian/ryan rather than alloy; VoiceDesign requires explicit `task_type=VoiceDesign` plus `instructions`. This produces the error `This model does not support CustomVoice task (no speakers configured)`.

The root cause is not a particular model: the product treats every `/v1/*` endpoint as an OpenAI protocol with fixed parameters. In reality, apart from chat/embeddings, **the same endpoint has different parameter semantics across engines and models**: speech has speaker/cloning/style task modes; images have model-specific size/steps/guidance; video has seconds/fps/resolution enums; realtime has session configuration; and chat also has model-specific options such as reasoning, tools, and multimodal content. Catalog entries such as voxcpm2, moss-tts, fish s2-pro, and higgs-audio encounter the same class of problem. Expert users also need to construct and edit requests without form constraints.

## Decision

Request assembly has four layers, each owning only one kind of knowledge. No layer may branch on a model or engine name:

1. **Protocol definitions (code, `inferenceapi.Definition`)**: describe only what an operation type is: allowed endpoints, methods, encoding, response type, and asynchronous lifecycle (already present), plus new **primary input slots** (chat→`messages`; embeddings→`input`; rerank→`query`+`documents`; transcribe→`file`; speech→`input`; image/video→`prompt`; realtime→event stream) and **discovery routes**: read-only GET routes that the Playground can query on the current deployment through the proxy, such as `/v1/audio/voices` for speech and `/v1/models` and `/openapi.json` for all types. The proxy's `AllowsRequest` allowlist also includes discovery routes (GET only).
2. **Catalog declarations (YAML spec `api.operations[]`)**: each operation has two optional fields. `request_defaults`: constants merged into the request body (JSON) or form (multipart), such as `task_type: Base` for Base and `task_type: VoiceDesign` for VoiceDesign. `inputs`: a list of extra fields the user must provide for the operation, each with `name`, `kind` (`text|number|select|json|audio|image|file`), `required`, localized `label`, `options`, and `default`; the frontend encodes media fields as base64 data URLs or multipart parts according to `encoding`. `NormalizeContract` validates that kind belongs to the fixed vocabulary and that `name` does not conflict with a primary input slot or `model`. These fields enter the deployment snapshot with the contract, keeping deployed instance behavior immutable.
3. **Engine discovery (runtime)**: the Playground uses the Definition's discovery routes to obtain facts from the deployment itself: the voices list populates dropdowns, and `/openapi.json` provides fields, types, enums, and defaults for the selected endpoint. Discovery failure only degrades functionality (dropdowns become free text, schema hints disappear); it does not block requests. **Code no longer guesses any defaults**: if there are no voices, do not send a `voice` field.
4. **User overrides (expert mode)**: every Playground page has a “Request” drawer showing the final assembled request: method, path, headers, and body (JSON is directly editable; multipart shows a field table and file slots). It offers “Generate skeleton from OpenAPI,” “Save as preset,” and “Copy curl / Python.” The page has mutually exclusive “Form” and “Raw” modes: in raw mode, text is the sole source of truth and the form is frozen; switching back regenerates the request from the form and warns that switching back discards manual edits. Raw requests pass unchanged through the same proxy, without any server patching, and endpoints remain restricted to the routes allowed by that deployment's contract. Chat's existing “Extra parameters JSON” is folded into this mechanism.

The merge order is fixed: primary input slots (renderer UI) → `request_defaults` → user values for `inputs` → expert raw edits.

Three consumers share the same assembly logic: the Playground, the example code generator, and template-probe L3 (model-specific fields such as `L3Case.Voice` become generic `request_overrides`). The Deployment DTO adds resolved contract operations (type, endpoint, renderer), and the deployment list and template details page share one `playgroundRoute(operation)` to determine the entry route.

## Alternatives and Reasons for Rejection

- **Engine/model adapters in the server**: adding each model would require changing Go code, violating the rule that the server supplies the parameter dictionary without hardcoding; rejected.
- **Purely OpenAPI-driven forms**: vLLM's `/v1/audio/speech` schema is the union of fields for all TTS models. It cannot express model-level constraints such as “Base requires ref_audio,” nor does it include localized wording. OpenAPI serves only as a source of expert-mode hints and skeletons; curated `inputs` remain the form's source of truth.
- **Only add an “Extra JSON” text box**: this cannot handle file inputs (ref_audio requires base64 or multipart) or fix entry routes; retain it as a subset of expert mode.

## Impact

- Compatibility: existing YAML works unchanged without the new fields; examples and L3 send only primary input slots when no declarations exist.
- Data: add declarations to 5 Qwen3-TTS specs; review the parameter semantics of other TTS/image/video specs in the catalog.
- Security: expert mode does not relax the proxy allowlist; only read-only discovery routes are added. External models (LLMs in Settings) still support chat only.
- Tests: `inferenceapi` validation and example snapshots; proxy allowlisting of discovery routes and rejection of non-GET requests; literal JSON round-trip for DTOs; RTL coverage of entry routes, declarative inputs rendering, discovery degradation, and unchanged raw-mode submission; template-probe L3 assembly.
- Follow-up: initially store per-user presets in localStorage. “Save as inference template” writes the current expert request back to `request_defaults` in a user-source spec, feeding expert usage back into the catalog.

<!-- translation-source-sha256: 37fbba5252f46ac7beca005130388b02a9fd63754b63eb8fd8a954e114b7d0fc -->
