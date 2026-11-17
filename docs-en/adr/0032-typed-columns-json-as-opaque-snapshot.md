# ADR 0032: Persist Facts in Typed Columns by Default; Use JSON Columns Only as Opaque Snapshots Read and Written as a Whole

Date: 2026-10-01 · Status: Accepted

Based on: [Typed Columns by Default for Persisted Facts](../../artifacts/archive/2026-10-01-store-typed-columns/plan.md).

## Context

Entities in `store` have more than seventy `type:text` columns, most containing JSON. Writing JSON into a text column requires no migration or schema change,
making it the path of least resistance for adding a fact. The cost is that this portion of the schema exists only in Go code: the database cannot constrain it, migrations cannot see it,
different writers can write different shapes to the same column, and each reader decodes it and handles bad data independently. Once a query needs to look inside JSON, SQL can only
use text matching or `json_extract`, with results that depend on serialization details.

## Decision

1. **Add persisted facts cautiously; storage format grants no exemption.** Treat a new JSON/text column or a new key in existing JSON the same as adding a column or table:
   add it only when genuinely necessary and impossible to infer or calculate from existing data.
2. **Use typed columns or tables by default.** JSON columns are only for opaque snapshots read and written as a whole: the database must not query, sort, join, or partially update their internal fields.
   Each JSON column corresponds to exactly one named Go type, documented in its column comment (`package.Type`; packages store cannot import are named only in comments).
   Do not duplicate facts already in other columns. Shape changes also require migration or read-time compatibility, with tests for old data. Generic catch-all containers such as `map[string]any` / `Extra` /
   `Meta` are forbidden, as are multiple shapes in one column. For non-JSON text columns, comments must specify whether they contain free text or ciphertext.
3. **Value versus relationship is the boundary between JSON and relational tables.** A column holding a *value* owned by its row, read and replaced as a whole with that row,
   can use JSON: a spec's weight sources (`inference_specs.weight_sources`) are descriptive data such as repository names and revisions,
   written and read together with the spec. A column holding *relationships between entities*, through which other entities must be found, is relational:
   a deployment's node list (`deployments.node_ids`) contains IDs of `nodes` rows. Eviction checks, gateway routing, and scheduling all need to answer
   which deployments use a given node. To decide, check whether the JSON contains identifiers of other rows: if so, it is a relationship.
4. **When an occasional reverse lookup of a value is needed, decode it using its owning type in the domain package; do not inspect JSON in SQL.** `inferencecatalog.ModelDeclaringWeightRepo`
   finds the catalog model that declares a weight repository: store reads only each spec's owning model ID and `weight_sources`
   columns. The domain package decodes and compares them with `inferencecatalog.DecodeWeightSources` (the column's sole decoding entry point), then loads only the matching model.
   This follows the same pattern as `monitor.rules`: store exposes only raw JSON, and parsing belongs to the domain package.
   The cost is explicit: in the built-in catalog (379 models, 1099 specs), this reverse lookup takes about 8 ms, versus about 1 ms for the SQL text matching it replaces.
   The reverse lookup must not load the spec's other columns (loading the entire catalog takes about 42 ms), nor be unknowingly repeated in a loop—it runs once per candidate node in deployment preview.
   If it becomes a bottleneck, the caller should parse only once per request rather than return to inspecting JSON in SQL.
5. **Two cases where no structure is added.**
   - Do not split `weight_sources` into a child table. Decision 7 in ADR 0009 specifies two tables, models + specs, with list filtering in SQL. Keeping spec JSON
     fields opaque to SQL is intentional; every place that loads a spec reads them as a whole. Only the reverse lookup above examines them, scanning roughly a thousand rows and reading just two columns.
     Splitting out a third table would require changes to all preloading and test literals without adding any capability.
   - Do not promote `state` / `phase` from `instance_events.data` to columns. Timeline collapsing uses `json_extract` to read these two fields, but new rows guarantee by construction
     that adjacent rows differ in state or phase (events are appended only when either changes). Collapsing applies only to historical progress rows written by older versions,
     which retention eventually removes. Permanently adding two columns for a transitional read-side transform is exactly what decision 1 is meant to prevent.
6. **Enforce this with three ratchet budgets** (`scripts/arch-constraints.md`): `store_text_columns` caps the total number of columns that can hold JSON
   (`type:text` columns plus fixed-length columns whose comments declare JSON). Adding one requires raising the cap and explaining in `# raised:` why it cannot be a typed column.
   `store_text_columns_undocumented` requires comments on all these columns. `store_sql_json_lines` caps SQL lines that inspect JSON outside migrations
   (JSON functions, `instr(`, and `->` / `->>` operators; comment lines do not count). The only current exception is the timeline collapsing above.
   The script cannot detect JSON in fixed-length columns whose comments do not mention it, or `LIKE` text matching on JSON columns; review must catch these.

## Consequences

- Under decision 3, `deployments.node_ids` is a relationship, but it is still JSON. Its decoding is consolidated in `store.Deployment.StoredNodeIDs`
  (`NodeIDList` falls back to the primary node on top of it), preserving each caller's existing handling of empty values and bad data. Whether to convert it to a join table remains undecided (`todo.md`).
- In addition to human-readable placement explanations, `instances.schedule_expl` carries launch facts used in subsequent decisions (compute network, PD endpoints, and unit).
  Migrations have used `json_set` to rewrite its internal fields. Whether to extract these facts remains undecided (`todo.md`).
- Once historical event rows are retired, remove the timeline collapsing SQL and this exception to `store_sql_json_lines` together (`todo.md`).
- When adding a persisted fact, review first asks whether it can be derived, then whether it can use a typed column. If JSON is chosen, the comment must identify its owning type, and the reason for raising the budget
  appears in the diff.
- This rule applies to every new or modified column. Existing columns that predate the rule and do not meet decision 2 are listed in `todo.md` and remain unchanged until addressed there;
  the budget blocks additions and does not endorse existing columns.

<!-- translation-source-sha256: 16653121ca8612b634913cfd885e4ebf86ab6f24947375866ad1ee9c067024d3 -->
