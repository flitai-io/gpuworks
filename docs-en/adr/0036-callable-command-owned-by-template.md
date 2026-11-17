# ADR 0036: Templates Own the Callable Command Line; Platform Facts Are Template Variables

Date: 2026-10-08 · Status: Accepted

## Context

The callable engine runs the Serve Adapter included in the catalog. Previously, it always prepended `--model <path> --host 0.0.0.0 --port <port>` to the template arguments,
requiring adapters to recognize these three flags.

Multi-node deployments need more platform facts: node count, this node's rank, the master address and port, and whether to run headless. These values exist only after the controller
assigns ranks for a specific launch and cannot be known when writing the template. The available template variables were limited to `GPU_COUNT`, `MODEL_PATH`, `TP_SIZE`,
and resolver output, and were expanded only when the spec contained placement. A branch once followed the vLLM/SGLang approach by having callable also append
flags such as `--tensor-parallel-size`, `--nnodes`, and `--node-rank`. This breaks every adapter that does not recognize these flags: the built-in catalog's
`gpuworks_serve.py` parses strictly with `argparse.parse_args()`, so even single-node deployments exit immediately because of the extra `--tensor-parallel-size 1`.
Another approach is for adapters to guess their own rank (for example, from the local IP), but that hard-codes the test environment into the template.

vLLM and SGLang have fixed vocabularies, so the platform knows what to inject. A callable command line is defined by the template author; whenever the platform appends anything in a fixed form,
it decides the adapter's CLI on the author's behalf.

## Decision

1. **Callable appends no arguments.** Its argv consists of template arguments plus user arguments; the executable is usually specified by the template's `entrypoint`.
2. **Platform facts are consistently exposed as template variables**, available to every engine: `MODEL_PATH`, `API_HOST`, `API_PORT`, `NNODES`, `NODE_RANK`,
   `HEADLESS`, `GPU_COUNT`, and `TP_SIZE`, plus `MASTER_ADDR` and `MASTER_PORT` for multi-node deployments. The vocabulary is defined only in `internal/server/placement/expand.go`,
   and values are calculated only in `deploy.ResolveLaunch` (`launch_variables.go`). Templates decide which variables to use and how to arrange them.
3. **Expansion no longer depends on placement** and runs at every launch.
4. **Resolvers cannot return platform facts**: nodes, ports, and ranks do not yet exist when the resolver runs, so any values it supplies can only be guesses.
5. **Missing variables cause failure**, with no silent fallback:
   - At deployment, unresolved names in argv or env cause an error.
   - At import, a spec without a resolver is rejected immediately if it references names outside the vocabulary.
   - Single-node deployments do not have `MASTER_*`.
   - `pre_start_script` runs in a shell; names outside the vocabulary are left for the shell.
6. **A callable spec must reference `${API_PORT}`.** The gateway and health checks access the port reserved by the platform, and only the template can communicate it to the adapter.
   This is checked at import and checked again for engine mode at deployment. A deployment's own command override takes over the entire command and is excluded from this check.

## Consequences

- The 7 callable profiles in the built-in catalog now explicitly begin their args with `--model ${MODEL_PATH} --host ${API_HOST} --port ${API_PORT}`,
  producing argv identical, character for character, to the previous result.
- For existing callable deployments, migration 130 (`callable_template_args`) prepends the same three variable pairs to the template arguments in their catalog selection snapshots,
  producing argv after restart identical, character for character, to the pre-upgrade result.
- User callable profiles already imported into the database that do not reference `${API_PORT}` are not migrated. New deployments based on them are explicitly rejected, requiring
  reimport of a corrected package. The old injection behavior is not retained as a compatibility mode.
- Managed writable directories (such as the KV disk tier) can currently be requested only through vLLM's argument dictionary, so callable cannot request them and no corresponding variables are exposed yet.
  When generic managed directories are added for callable, add paired "path + capacity limit" entries to the vocabulary.

<!-- translation-source-sha256: cbae84292f573126fc66cc70f86110b341dca3c9d605236ff16ab00b62f1ef55 -->
