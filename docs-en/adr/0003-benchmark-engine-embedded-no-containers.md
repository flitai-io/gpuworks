# ADR 0003: Embed the Benchmark Engine in the Binaries Without External Tools or Containers

This replaces the original EvalScope container approach: the load generator is embedded in the server and agent binaries as a pure Go library (`pkg/bench`). Generating load requires no image pulls, containers, or Python. Lessons from the previous approach: result parsing relied on Python embedded in the container with hardcoded EvalScope field names, so upstream field changes could silently produce all-zero results; image pulls were slow and unavailable in offline environments; execution was all-or-nothing, with neither progress nor partial results. The cost is implementing load generation, metric accumulation, and fixed-length input synthesis ourselves (with two fallback levels: a precise mode using a pure Go tokenizer and a corpus estimation mode). In return, we fully control the result schema, and support staff can SSH into a node and run standalone benchmarks with `gpuworks-agent bench`.

The old container-based jobs pipeline (`store.Job`, the agent jobEngine, and the evalscope plugin) is removed in full. Retrieve it from Git history if container-based tasks are needed.

<!-- translation-source-sha256: 22f9a7a4ae72120a531c94f745d1153d159a4dfe8c3ed41e03c12b64d1bfffe8 -->
