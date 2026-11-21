# User Glossary

This is a concise glossary of user-facing terms. For complete definitions and counterexamples, refer to [CONTEXT.md](../../CONTEXT.md) in the root directory.

| Term | Meaning |
|---|---|
| Node | A host or container that runs gpuworks-agent, reports GPUs, and hosts instances |
| Instance | An inference service process/container that a deployment runs on a node |
| Original model | The stable identity of a model, used for aggregation and deduplication across engines (such as `deepseek-ai/deepseek-v4`) |
| Spec | An executable combination of engine, image, weights, GPU predicates (GPU matching conditions), and parameters |
| Scenario | A label describing a spec's intended use, such as low latency, balanced, or high throughput |
| Presence | The state of an image/model on a node, such as absent, pulling, or present |
| Acquisition | The process by which weights and images arrive on a node. When starting a replica, the node decides based on pull settings whether to use an existing local copy, copy within a sync group, pull, or download; the deployment does not choose the source |
| Peer sync | Direct transfer of models or images from an online node with a complete copy, without relaying through the server; happens automatically within sync groups and can also be initiated manually |
| Pull settings | Source rules (image rules and model site rules), proxies, sync groups, and credentials that determine where nodes pull images and download models, which proxy they use, and which peers they can copy from |
| Site rule | A source rule whose `match` starts with `http(s)://`: it matches a model site's root (such as `https://huggingface.co`), and its alternative address can include a path. Rules are cumulative, fallback to the original source can be disabled, and "Used by the management node for model metadata" can be selected |
| Pull proxy | An ordered proxy entry in pull settings. A node uses the first entry whose label requirements it meets; the management node uses the first entry with "Used for management node outbound access" selected |
| Pull candidate | A candidate registry reference for the same image or a candidate download address for the same model (alternative addresses from source rules come first, followed by the image's own replicas or the site itself). Candidates are tried in order until one succeeds; failed addresses enter a 30-minute cooldown |
| Evaluation | The umbrella concept for measuring a service; currently includes benchmarks |
| Benchmark | Applies a controlled load to an OpenAI-compatible endpoint and records performance metrics |
| Deviated | An informational state indicating that you have overridden template fields; not an error |
| Quick/Expert mode | Two views of the same deployment page: Quick mode shows only preselected common fields (an allowlist-based form), while Expert mode provides full parameter editing |
| Weighted QoS | Backends such as PPU allocate throughput according to quotas when busy; this does not mean hardware partitioning |

<!-- translation-source-sha256: d5cc6029e2baa7ab7c33d87cb58817397dc1c5daa636c69eef10475e52ab13a2 # User Glossary -->
