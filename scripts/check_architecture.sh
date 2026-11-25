#!/bin/sh
set -eu

# Architecture gate for artifacts/archive/2026-09-09-architecture-cleanup/plan.md.
#
# Three independent parts:
#   1. Budgets — every key in scripts/arch-constraints.txt is measured on the current
#      tree and compared against its recorded limit. A "max" key may only ever
#      shrink, a "min" key may only ever grow (ratchet). Slack on a "max" key is
#      a WARNING, not a failure: it means the budget should be tightened in the
#      same MR that produced the improvement.
#   2. Budget regressions vs baseline — the ratchet in part 1 is a promise about
#      this tree in isolation; this part makes it mechanical by diffing
#      scripts/arch-constraints.txt against the copy at the merge base with
#      origin/master. A "max" or "warn" key whose limit grew, or a "min" key
#      whose limit shrank, fails unless scripts/arch-constraints.md records it
#      in a `- 放宽` line naming a Jira ticket and the condition for bringing
#      the limit back. New or removed keys are not a regression. When no
#      baseline can be resolved (shallow clone, no origin remote, already on
#      the default branch) this prints a NOTE and is skipped, never failed.
#      A third direction, "warn", is a soft budget: over the limit prints a
#      WARNING and never fails.
#      ui_bundle_gzip_kb is a warn key measured by scripts/check_ui_bundle_size.sh
#      after a UI build; this script only skips it.
#   3. Dependency direction — hard structural rules (plan §5.3). These are not
#      budgets: any violation fails immediately.
#
# Intent, exact command and exclusion list per key: scripts/arch-constraints.md

cd "$(dirname "$0")/.."

BUDGET_FILE="scripts/arch-constraints.txt"
BUDGET_DOC="scripts/arch-constraints.md"
if [ ! -f "$BUDGET_FILE" ]; then
  echo "missing $BUDGET_FILE" >&2
  exit 1
fi

if command -v node >/dev/null 2>&1; then
  ./scripts/check_ui_architecture.sh
else
  echo "NOTE: node not found; UI architecture checks run separately in the UI build"
fi

# Every import-graph measurement below reads `go list -e ... 2>/dev/null`. -e
# keeps per-package errors (a missing webui dist) out of the exit status, but a
# toolchain that cannot load the module at all — older than go.mod's go line
# under GOTOOLCHAIN=local — then yields empty output: import counts drop to
# zero and waivers look stale, reported as violations that are not there.
# Stop on that up front with go's own error.
if ! go_list_err=$(go list -e -f '{{.ImportPath}}' ./... 2>&1 >/dev/null); then
  echo "go list cannot load the module, so the import-graph checks cannot run:" >&2
  printf '%s\n' "$go_list_err" | sed 's/^/  /' >&2
  exit 1
fi

failures=0
warnings=0
waivers=0

# --- source file sets ------------------------------------------------------
# Production Go: internal/, pkg/, cmd/ minus *_test.go.
go_prod_files() {
  find internal pkg cmd -type f -name '*.go' ! -name '*_test.go' | sort
}

# Console overlay modules a separately built console copies into
# ui/src/extensions/<id>/ are git-ignored and not this repository's code, so no
# UI measurement counts them; ee_ui_internal_imports alone looks at them, and
# the console overlays section refuses tracked files there. drop_ui_overlays
# removes their paths (or "path:line:..." hits) from a list on stdin.
drop_ui_overlays() {
  grep -vE '^ui/src/extensions/[^/]+/' || true
}

# Production UI sources: ui/src minus *.test.* (and minus the i18n locale JSON,
# which is translated text rather than code, and the console overlays).
ui_prod_files() {
  find ui/src -type f \( -name '*.ts' -o -name '*.tsx' -o -name '*.css' \) \
    ! -name '*.test.ts' ! -name '*.test.tsx' | drop_ui_overlays | sort
}

# Production store sources: internal/server/store minus *_test.go.
store_prod_files() {
  find internal/server/store -type f -name '*.go' ! -name '*_test.go' | sort
}

# store_json_column_lines prints "<documented|undocumented>\t<file>:<line>" for
# every store column that can hold JSON: each struct field whose gorm tag says
# type:text, plus each other gorm-tagged struct field whose comment declares
# JSON (the word JSON in its own // comment, or in the comment line right above
# it). A column is documented when its own line carries a // comment or the line
# above it is a comment line. Both store_text_columns and
# store_text_columns_undocumented read this one list; see
# scripts/arch-constraints.md for what it cannot see.
store_json_column_lines() {
  store_prod_files | xargs awk '
    FNR == 1 { prev = "" }
    {
      own = ""
      if (index($0, "//") > 0) own = substr($0, index($0, "//"))
      above = (prev ~ /^[[:space:]]*\/\//) ? prev : ""
      field = ($0 ~ /^[[:space:]]+[A-Za-z_][A-Za-z0-9_]*[[:space:]]+[^\/]*`[^`]*gorm:"/)
      json = (own ~ /(^|[^A-Za-z])JSON([^A-Za-z]|$)/ || above ~ /(^|[^A-Za-z])JSON([^A-Za-z]|$)/)
      if (field && ($0 ~ /type:text/ || json)) {
        print ((own != "" || above != "") ? "documented" : "undocumented") "\t" FILENAME ":" FNR
      }
      prev = $0
    }'
}

# store_sql_json_hits prints "file:line:text" for every line of a store source
# (tests and migration*.go excluded) that looks into JSON from SQL: a SQLite
# JSON function call (json_extract, json_valid, json_each, json_tree, json_type,
# json_set, ...), instr(, or the -> / ->> operator followed by a quoted path, a
# bound parameter or an index. Comment lines do not count: they cannot run a
# query. See scripts/arch-constraints.md for the exact pattern and its limits.
store_sql_json_hits() {
  store_prod_files | grep -v '/migration[^/]*\.go$' \
    | xargs grep -nE "json_[a-z_]+[[:space:]]*\(|instr[[:space:]]*\(|->>?[[:space:]]*('|\\?|[0-9])" 2>/dev/null \
    | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' || true
}

# edition_branch_hits prints "file:line:text" for every production Go line that
# branches on the edition: a comparison with version.Edition (either operand
# order) or a switch on it, which covers every case of that switch. The open
# source tree only displays the edition; behaviour that differs by edition
# belongs to the separately built server that supplies it. Comment lines do
# not count.
edition_branch_hits() {
  go_prod_files \
    | xargs grep -nE 'version\.Edition[[:space:]]*(==|!=)|(==|!=)[[:space:]]*version\.Edition|switch[[:space:]]+version\.Edition' 2>/dev/null \
    | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' || true
}

# go_imports prints "<import-path>\t<space separated imports>" for every package
# in the module. `go list` without -e fails on this repo whenever
# internal/server/webui/dist is absent (the go:embed pattern matches nothing),
# so -e is mandatory here; the webui package itself imports no first-party
# package, so its degraded entry does not affect any measurement.
go_imports() {
  go list -e -f '{{.ImportPath}}	{{join .Imports " "}}' ./... 2>/dev/null
}

# untested_shared_ui lists the shared UI building blocks no test covers by
# name: ui-kit compositions, the console shell and the page structure every
# page reuses (PageHeader, EmptyState, ResourceTable, DetailDrawer). A test
# counts when it imports the file, or names its ui/src/... path because it
# drives it through its real host (the sidebar through App, a column filter
# through ResourceTable); a page test that happens to render it does not.
untested_shared_ui() {
  { find ui/src/ui-kit ui/src/app ui/src/components/detail ui/src/components/resourceTable \
      -type f \( -name '*.ts' -o -name '*.tsx' \) 2>/dev/null
    printf '%s\n' ui/src/components/PageHeader.tsx ui/src/components/EmptyState.tsx
  } | sort | while IFS= read -r f; do
    [ -f "$f" ] || continue
    module=${f#ui/}; module=${module%.*}
    grep -rqE "['\"](\.\./)+${module}(\.tsx?)?['\"]|ui/${module}\.tsx?" ui/tests --include='*.ts' --include='*.tsx' 2>/dev/null || echo "$f"
  done
}

# decl_count looks in the non-test Go files of package directory $1 for the
# top-level declaration matching extended regular expression $2 and counts the
# lines from it to its closing brace that match $3 (every line when $3 is
# empty; with $4 = body the declaration line itself is skipped, for a struct's
# fields). It prints NOTFOUND when no file declares it, which the budget loop
# fails: a declaration moved to another package must take its budget along.
# When several files declare it (build-tag variants), the first by name wins.
# Patterns go through the environment because awk -v strips their backslash
# escapes, and use [[:blank:]] rather than \t, which busybox awk does not
# expand in a dynamic pattern.
decl_count() {
  for f in "$1"/*.go; do
    case "$f" in *_test.go) continue ;; esac
    DECL="$2" LINE="$3" BODY="${4:-}" awk '
      !inside && $0 ~ ENVIRON["DECL"] {inside=1; if (ENVIRON["BODY"] == "body") next}
      inside && /^}/ {if (ENVIRON["LINE"] == "") n++; found=1; exit}
      inside && (ENVIRON["LINE"] == "" || $0 ~ ENVIRON["LINE"]) {n++}
      END {if (found) print n+0}' "$f" 2>/dev/null
  done | head -n 1 | grep . || echo NOTFOUND
}

# controller_struct_count counts the Controller struct's lines matching $1.
controller_struct_count() {
  decl_count internal/server/controller '^type Controller struct' "$1" body
}

# --- measurements ----------------------------------------------------------
measure() {
  key="$1"
  case "$key" in
  pending_removal_literal)
    grep -rn '"pending-removal"' internal --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' \
      | grep -v '^internal/proto/instance\.go:' \
      | grep -c . || true
    ;;
  deployment_state_literal)
    {
      grep -rnE 'dep(loyment)?\.State (==|!=) "[a-z-]+"' internal --include='*.go' 2>/dev/null \
        | grep -v '_test\.go' || true
      grep -rnE 'SetDeploymentState\([^)]*"[a-z-]+"' internal --include='*.go' 2>/dev/null \
        | grep -v '_test\.go' || true
    } | grep -c . || true
    ;;
  engine_literal_server)
    grep -rlE '"vllm"|"sglang"' internal pkg cmd --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' \
      | grep -v '^internal/engines/engines\.go$' \
      | grep -v '^internal/server/deploy/engine\.go$' \
      | grep -v '^pkg/enginelog/' \
      | grep -c . || true
    ;;
  engine_literal_ui)
    grep -rnE "'vllm'|\"vllm\"|'sglang'|\"sglang\"" ui/src \
      --include='*.ts' --include='*.tsx' 2>/dev/null \
      | grep -v '\.test\.' | grep -v '^ui/src/i18n/' | drop_ui_overlays | grep -c . || true
    ;;
  ui_antd_import_files)
    # Count source files, including re-exports, dynamic imports and subpaths.
    grep -rlE "['\"]antd(/[^'\"]*)?['\"]" ui/src \
      --include='*.ts' --include='*.tsx' 2>/dev/null \
      | grep -v '\.test\.' | drop_ui_overlays | grep -c . || true
    ;;
  ui_antd_icon_import_files)
    grep -rlE "['\"]@ant-design/icons(/[^'\"]*)?['\"]" ui/src \
      --include='*.ts' --include='*.tsx' 2>/dev/null \
      | grep -v '\.test\.' | grep -v '^ui/src/components/ui/' | drop_ui_overlays | grep -c . || true
    ;;
  ui_untested_shared_files)
    untested_shared_ui | grep -c . || true
    ;;
  ui_copy_*_strings | ui_copy_inline_help_refs | ui_common_locale_keys | ee_ui_internal_imports)
    echo "UI"
    ;;
  ui_bundle_gzip_kb)
    # Needs a fresh UI build; measured by scripts/check_ui_bundle_size.sh.
    echo "EXTERNAL"
    ;;
  setting_key_literal)
    grep -rnE '(Get|Set)Setting\("' internal --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' | grep -v '^internal/server/store/settings_keys\.go:' \
      | grep -c . || true
    ;;
  vendor_normalize_funcs)
    grep -rnE 'func (normalizeVendor|NormalizeVendor)\(' internal pkg cmd --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' | grep -c . || true
    ;;
  agent_runtime_name_comparisons)
    grep -rnE '(==|!=) *("(docker|nerdctl|udocker|kube|host)"|(agent)?runtime\.Driver[A-Z]|proto\.RuntimeDriver|kube\.DriverName|hostproc\.DriverName|accelerator\.Runtime(Docker|Nerdctl|Udocker|Kube|Host))|("(docker|nerdctl|udocker|kube|host)"|(agent)?runtime\.Driver[A-Z]|proto\.RuntimeDriver|kube\.DriverName|hostproc\.DriverName|accelerator\.Runtime(Docker|Nerdctl|Udocker|Kube|Host)) *(==|!=)|case .*("(docker|nerdctl|udocker|kube|host)"|(agent)?runtime\.Driver[A-Z]|proto\.RuntimeDriver|kube\.DriverName|hostproc\.DriverName|accelerator\.Runtime(Docker|Nerdctl|Udocker|Kube|Host))|(EqualFold|Contains)\(.*("(docker|nerdctl|udocker|kube|host)"|(agent)?runtime\.Driver[A-Z]|proto\.RuntimeDriver|kube\.DriverName|hostproc\.DriverName|accelerator\.Runtime(Docker|Nerdctl|Udocker|Kube|Host))' \
      internal/agent/gpu internal/agent/engine --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' | grep -c . || true
    ;;
  agent_container_option_literals)
    grep -rnE '"--(device|gpus|runtime|group-add|tmpfs)"' internal/agent/gpu internal/agent/engine --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' | grep -c . || true
    ;;
  vendor_name_files_outside_definitions)
    vendor_names=$(sed -nE 's/^[[:space:]]*Vendor[A-Za-z0-9]+[[:space:]]*=[[:space:]]*"([a-z0-9-]+)"/\1/p' internal/accelerator/vendor.go | paste -sd'|' -)
    grep -rlE "accelerator\.Vendor[A-Z][A-Za-z0-9]*([^A-Za-z0-9(]|\$)|(==|!=) *\"($vendor_names)\"|\"($vendor_names)\" *(==|!=)|case .*\"($vendor_names)\"|(EqualFold|Contains|HasPrefix)\(.*\"($vendor_names)\"" internal cmd pkg --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' | grep -v '^internal/accelerator/' | grep -v '^internal/agent/gpu/' | grep -c . || true
    ;;
  udocker_vendor_inference_funcs)
    grep -rhE '^func is[A-Z][A-Za-z0-9]*Spec\(' internal/agent/runtime/udocker --include='*.go' --exclude='*_test.go' 2>/dev/null \
      | grep -c . || true
    ;;
  infer_vendor_callers)
    grep -rn 'deploy\.InferVendor(' internal pkg cmd --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' | grep -c . || true
    ;;
  store_list_nodes_callers)
    grep -rnE '\.ListNodes\(\)' internal pkg cmd --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' | grep -v '^internal/testenv/' | grep -c . || true
    ;;
  store_list_deployments_callers)
    grep -rnE '\.ListDeployments\(\)' internal pkg cmd --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' | grep -v '^internal/testenv/' | grep -c . || true
    ;;
  store_list_instances_callers)
    grep -rnE '\.ListInstances\(\)' internal pkg cmd --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' | grep -v '^internal/testenv/' | grep -c . || true
    ;;
  store_concrete_importers)
    go_imports \
      | grep -v '\.test$' \
      | grep -v '^gpuworks\.ai/gpuworks/internal/integrationtest/gpuallocation/internal/fixtures[[:space:]]' \
      | awk -F'\t' '$2 ~ /gpuworks\/internal\/server\/store( |$)/ {print $1}' \
      | sort -u | grep -c . || true
    ;;
  store_db_accessor_callers)
    grep -rnE '\.DB\(\)' internal cmd pkg scripts test --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' | grep -v '^internal/server/store/' | grep -c . || true
    ;;
  controller_struct_fields)
    # One per field line (a, b int is one), embedded fields included.
    controller_struct_count '^[[:blank:]][*A-Za-z_]'
    ;;
  controller_struct_mutexes)
    controller_struct_count '^[[:blank:]](\*?sync\.(RW)?Mutex|[A-Za-z_][A-Za-z0-9_, ]*[[:blank:]]+sync\.(RW)?Mutex)'
    ;;
  controller_struct_maps)
    controller_struct_count '^[[:blank:]][A-Za-z_][A-Za-z0-9_, ]*[[:blank:]]+map\['
    ;;
  agent_node_runtime_fields | agent_node_runtime_mutexes | agent_node_runtime_methods | agent_onrpc_cases)
    m=${key#agent_node_runtime_}
    [ "$key" = agent_onrpc_cases ] && m=rpcs
    CGO_ENABLED=0 go run ./scripts/structcount -dir internal/agent/app -type NodeRuntime -measure "$m" 2>/dev/null || echo NOTFOUND
    ;;
  agent_image_deps_fields)
    CGO_ENABLED=0 go run ./scripts/structcount -dir internal/agent/app -type imageDeps -measure fields 2>/dev/null || echo NOTFOUND
    ;;
  agent_engine_struct_maps)
    CGO_ENABLED=0 go run ./scripts/structcount -dir internal/agent/engine -type Engine -measure maps 2>/dev/null || echo NOTFOUND
    ;;
  controller_exported_methods)
    grep -rnE '^func \(c \*Controller\) [A-Z]' internal/server/controller --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' | grep -c . || true
    ;;
  planning_concrete_store_refs)
    grep -rnE '\*store\.Store' internal/server/planning --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' | grep -c . || true
    ;;
  proxy_store_handle_refs)
    # The gateway holds plane.Source, never the store itself or its bus.
    grep -nE '\*store\.Store|store\.Open(With)?\(|\.Bus\b' internal/server/proxy/*.go internal/server/proxy/plane/*.go 2>/dev/null \
      | grep -v '_test\.go' | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' | grep -c . || true
    ;;
  proxy_bus_hub_imports)
    grep -nE '"gpuworks\.ai/gpuworks/internal/server/(bus|hub)"' internal/server/proxy/*.go internal/server/proxy/plane/*.go 2>/dev/null \
      | grep -v '_test\.go' | grep -c . || true
    ;;
  proxy_adapter_refs)
    # The adapter embeds the store: gateway code that imported it would be one
    # type assertion from the handle. Only the adapter itself and tests may.
    grep -nE '"gpuworks\.ai/gpuworks/internal/server/proxy/storeplane"|storeplane\.' internal/server/proxy/*.go internal/server/proxy/plane/*.go 2>/dev/null \
      | grep -v '_test\.go' | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' | grep -c . || true
    ;;
  proxy_store_type_files)
    grep -lE '"gpuworks\.ai/gpuworks/internal/server/store"' internal/server/proxy/*.go internal/server/proxy/plane/*.go 2>/dev/null \
      | grep -v '_test\.go' | grep -c . || true
    ;;
  planning_sync_files)
    grep -rlE '^[[:space:]]*"sync(/atomic)?"|^import "sync(/atomic)?"' internal/server/planning --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' | grep -c . || true
    ;;
  app_new_lines)
    decl_count internal/server/app '^func New\(' ''
    ;;
  app_run_lines)
    decl_count internal/server/app '^func \(a \*App\) Run\(' ''
    ;;
  app_run_goroutines)
    # goBG is Run's alias for bg.start; a bare go statement counts too.
    decl_count internal/server/app '^func \(a \*App\) Run\(' '^[[:blank:]]+(goBG\(|bg\.start\(|go[[:blank:]])'
    ;;
  app_prod_lines)
    find internal/server/app -maxdepth 1 -type f -name '*.go' ! -name '*_test.go' -exec cat {} + | grep -c '' || true
    ;;
  api_fanout)
    go list -e -f '{{join .Imports "\n"}}' ./internal/server/api 2>/dev/null \
      | grep '^gpuworks\.ai/gpuworks/' | sort -u | grep -c . || true
    ;;
  api_deps_fields)
    awk '/^type Deps struct/ {inside=1; next}
         inside && /^}/ {exit}
         inside && /^\t[A-Z]/ {n++}
         END {print n+0}' internal/server/api/dependencies.go
    ;;
  api_opaque_response_fields)
    CGO_ENABLED=0 go run ./scripts/apiconv -opaque 2>/dev/null || echo 999
    ;;
  api_goroutines)
    grep -rnE '^[[:space:]]+go (a\.|func)' internal/server/api --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' | grep -c . || true
    ;;
  store_domain_imports)
    grep -rnE '"gpuworks\.ai/gpuworks/(pkg/bench|pkg/topoparse|pkg/modelrepo|internal/server/imagecatalog/manifest|internal/server/monitor)' \
      internal/server/store --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' | grep -c . || true
    ;;
  agent_direct_exec)
    grep -rn 'exec\.Command' internal/agent/app --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' | grep -c . || true
    ;;
  bare_time_now_ctrl_hub)
    grep -rn 'time\.Now()' internal/server/controller internal/server/hub --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' | grep -c . || true
    ;;
  files_over_600)
    go_prod_files | while IFS= read -r f; do wc -l < "$f"; done \
      | awk '$1 > 600 {n++} END {print n+0}'
    ;;
  milestone_named_files)
    go_prod_files | while IFS= read -r f; do
      b=$(basename "$f")
      if echo "$b" | grep -qE '(^|[^0-9a-z])[msgp][0-9]{1,2}([^0-9a-z]|$)'; then
        echo "$f"
      fi
    done | grep -c . || true
    ;;
  process_name_refs)
    process_name_hits | grep -c . || true
    ;;
  dto_test_lines_min)
    cat internal/server/api/dto/*_test.go 2>/dev/null | grep -c '' || true
    ;;
  store_text_columns)
    store_json_column_lines | grep -c . || true
    ;;
  store_text_columns_undocumented)
    store_json_column_lines | grep -c '^undocumented' || true
    ;;
  store_sql_json_lines)
    store_sql_json_hits | grep -c . || true
    ;;
  edition_branches)
    edition_branch_hits | grep -c . || true
    ;;
  error_text_classification)
    error_text_classification_hits | grep -c . || true
    ;;
  instance_failed_without_reason)
    instance_failed_without_reason_hits | grep -c . || true
    ;;
  gateway_error_bypass)
    gateway_error_bypass_hits | grep -c . || true
    ;;
  legacy_check_vocabulary)
    legacy_check_vocabulary_hits | grep -c . || true
    ;;
  console_unlinked_code_packages)
    console_unlinked_code_package_links | grep -c . || true
    ;;
  kv_backend_id_literals)
    # A KV offload backend id is a row of internal/server/kvbackend and is
    # rendered only by the adapter's kv_render_* files (ADR 0069); anywhere
    # else, server or UI, it is a hard-coded backend rule.
    {
      grep -rnE '"(lmcache|mooncake_store|hicache)"' internal pkg cmd --include='*.go' 2>/dev/null \
        | grep -v '_test\.go' \
        | grep -v '^internal/server/kvbackend/' \
        | grep -v '^internal/server/deploy/kv_render_' || true
      grep -rnE "['\"](lmcache|mooncake_store|hicache)['\"]" ui/src \
        --include='*.ts' --include='*.tsx' 2>/dev/null \
        | grep -v '\.test\.' || true
    } | grep -c . || true
    ;;
  feature_key_literals_in_core)
    # A feature key belongs to the package that declares it (a manifest's
    # features, or a compiled module's declaration); core code never names
    # one. Counts quoted key-shaped literals handed to the gate (Require,
    # Admit, Access, Declared, Availability), converted to featuregate.Key,
    # assigned to a featuregate.Key constant, set as a route's Feature, or
    # given as a feature declaration's Key in a file importing featuregate.
    feature_key_literal_hits | grep -c . || true
    ;;
  published_tags_outside_grammar)
    # Published runner tags outside the release tag grammar. The image
    # manifest generator rejects such a tag unless its matrix entry declares
    # a legacy_reason, and rejects the declaration on a conforming tag, so
    # the declarations are the exceptions. Counts every mention outside
    # comment lines, whatever YAML style it is written in.
    grep -v '^[[:space:]]*#' build/runner-images/matrix.yaml | grep -o 'legacy_reason' | grep -c . || true
    ;;
  *)
    # `measure` runs inside a command substitution, so exiting here would only
    # kill the subshell. Emit a sentinel the caller turns into a failure.
    echo "UNKNOWN"
    ;;
  esac
}

# feature_key_literal_hits prints one line per feature key named by a
# literal in non-test core code (scripts/arch-constraints.md).
feature_key_literal_hits() {
  local key='"[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*){0,2}"'
  grep -rnE "(featuregate\.Key\(|\.(Require|Admit|Access|Declared|Availability)\()${key}|featuregate\.Key[[:space:]]*=[[:space:]]*\"|Feature:[[:space:]]*${key}" \
    internal cmd --include='*.go' 2>/dev/null \
    | grep -v '_test\.go:' || true
  # A feature declared with a literal key (featuregate.Feature{Key: "…"}),
  # in any file that imports featuregate, since the literal often sits on
  # its own line.
  grep -rlE '"gpuworks\.ai/gpuworks/internal/server/featuregate"' internal cmd --include='*.go' 2>/dev/null \
    | grep -v '_test\.go$' \
    | xargs -r grep -nHE "(^|[^A-Za-z0-9_])Key:[[:space:]]*${key}" 2>/dev/null || true
}

# ADR 0067 guards: failures and check results are codes, never prose that a
# later reader has to parse. Each *_hits function prints one line per
# violation; exclusions and the external system each allowlisted adapter wraps
# are listed in scripts/arch-constraints.md.
error_text_classification_hits() {
  grep -rnE 'strings\.(Contains|HasPrefix|HasSuffix|EqualFold|Index|LastIndex|Cut|CutPrefix)\((strings\.(ToLower|TrimSpace)\()?[A-Za-z_.]*([Ee]rr[A-Za-z]*|[Mm]sg|[Mm]essage|[Dd]etail|[Rr]eason|[Ss]tderr|[Oo]utput)\b|strings\.ToLower\([A-Za-z_.]*\.Error\(\)\)|\.Error\(\) *(==|!=)|switch [A-Za-z_.]*\.Error\(\)|MatchString\([^)]*\.Error\(\)' \
    internal pkg cmd --include='*.go' 2>/dev/null \
    | grep -v '_test\.go:' \
    | grep -vE '^(internal/agent/modelfetch/retry\.go|internal/agent/slurm/node_state\.go|internal/agent/runtime/docker/cli_answers\.go|internal/server/logstore/query\.go|internal/server/uxstore/query\.go|internal/server/provider/compshare/api_error\.go|internal/server/provider/vast/vast\.go|internal/testenv/cloudprovider/aliyun_api\.go|internal/testenv/cloudprovider/aliyun_infra\.go|internal/integrationtest/fleet/gateway\.go|internal/server/hub/benchmark_failure_code\.go|internal/server/store/instance_runtime_failure\.go|internal/server/provision/ssh_failure\.go|internal/server/provision/install_command\.go|internal/server/provision/probe\.go|internal/server/inferencecatalog/package_tolerant\.go):' || true
}
instance_failed_without_reason_hits() {
  grep -rnE 'State: *proto\.InstFailed|SetInstanceState\([^)]*proto\.InstFailed|reportState(Progress)?\([^)]*proto\.InstFailed|"state": *proto\.InstFailed|Update(Column)?\("state", *proto\.InstFailed|IsEnginePortRefusal|HasPrefix\([^)]*EnginePort|EnginePort(Refused|InUse)\+":"' \
    internal cmd --include='*.go' 2>/dev/null \
    | grep -v '_test\.go:' | grep -v '^internal/agent/engine/runtime_failure\.go:' || true
}
gateway_error_bypass_hits() {
  grep -rn 'writeErr(' internal/server/proxy --include='*.go' 2>/dev/null | grep -v '_test\.go:' || true
  grep -rn '"gpuworks_proxy"' internal test ui/src --include='*.go' --include='*.ts' --include='*.tsx' --include='*.py' 2>/dev/null || true
  grep -rn 'http\.Error(' internal/server/api internal/server/proxy internal/server/hub internal/server/batch --include='*.go' 2>/dev/null \
    | grep -v '_test\.go:' | grep -v '^internal/server/api/security_headers\.go:' || true
  grep -rnE '`\{"error"|map\[string\](any|string)\{"error":' internal/server --include='*.go' 2>/dev/null \
    | grep -v '_test\.go:' | grep -vE '^internal/server/(messages/http|api/responses|api/inference_templates_official)\.go:' || true
}
legacy_check_vocabulary_hits() {
  grep -rnE '(DoctorFinding|CardCheck|CardCheckRun)\{[^}]*(Status|Code|ID): "' internal pkg cmd --include='*.go' 2>/dev/null | grep -v '_test\.go:' || true
  grep -rnE 'DoctorLevel|accelerator\.Check(Pass|Warn|Fail|Skip|Pending|NotApplicable|Reason)|CheckStateFor|\.Level != proto' \
    internal/agent internal/server/cardhealth internal/server/diagnostics internal/proto --include='*.go' 2>/dev/null \
    | grep -v '_test\.go:' | grep -v '^internal/proto/check_compat\.go:' || true
  grep -rn '"pending"' internal/server/cardhealth internal/agent/gpu internal/accelerator internal/proto/card_check.go internal/proto/doctor.go --include='*.go' 2>/dev/null \
    | grep -v '_test\.go:' || true
  grep -rnE 'Status: *"(ok|error|warn|passed|failed|skipped)"|\.Status *[!=]= *"(ok|error|warn)"|status *:?= *"(ok|error)"' \
    internal/server/provision internal/server/provider internal/server/api/provision.go internal/server/api/provider_install.go --include='*.go' 2>/dev/null \
    | grep -v '_test\.go:' || true
  grep -rnE '\bInstallCheck\b|CheckResult\{Name|check\.Name ==' internal/server/provision internal/server/provider internal/server/api --include='*.go' 2>/dev/null | grep -v '_test\.go:' || true
  grep -nE 'Status(:| =| ==| !=) *"(passed|failed|blocked|unknown|pending|not_applicable|ok|error)"|status(, reason)?(, message)? :?= "(passed|failed|blocked|unknown|pending|not_applicable)"' \
    internal/server/deployment/*.go internal/server/api/diagnostic_projection.go internal/server/api/preflight.go 2>/dev/null | grep -v '_test\.go:' || true
  grep -rnE '"(info|incomplete|completed|unavailable)"' internal/server/diagnostics/doctor_report.go internal/server/diagnostics/node_doctor.go internal/server/diagnostics/node_connection.go 2>/dev/null || true
}
console_unlinked_code_package_links() {
  for main in ./cmd/gpuworks ./cmd/gpuworks-agent; do
    CGO_ENABLED=0 go list -deps "$main" 2>/dev/null | grep -E '/internal/(templateprobe|testenv/extplugin/fakeplugin)$' || true
  done
}

# process_name_hits prints "file:line:text" for every production source line
# carrying a process/milestone-style label (M1, S0, G2, P3 …) plus every
# production file whose *name* carries one.
#
# The bare pattern collides with legitimate vocabulary, so each source line is
# first passed through a neutralizer that blanks out the known-good tokens.
# Every exclusion below is listed in scripts/arch-constraints.md; keep the two in
# sync, and fix the pattern rather than raising the budget when a new
# false positive shows up.
neutralize_known_tokens() {
  # $1 = file path, so path-scoped exclusions can be applied.
  case "$1" in
  internal/agent/runtime/udocker/* | internal/agent/runtime/runveil/*)
    # udocker's PRoot execution mode is literally named P1. It is an external
    # tool's vocabulary, not a milestone; runveil reproduces that mode and
    # reports it to the guest under the same name.
    sed -E 's/[Pp]1/P_UDOCKER_EXECMODE/g'
    ;;
  internal/accelerator/identity.go | internal/agent/gpu/apple*.go | internal/server/hardwarecatalog/catalog.go)
    # Apple names its silicon generations M1 to M6 ("Apple M4 Pro"). The files
    # that model Apple hardware spell them verbatim; they are hardware
    # vocabulary, not milestones, so the exclusion is path-scoped to them like
    # the Tesla P40 case below.
    sed -E 's/M([1-9])([^0-9]|$)/APPLE_SOC\2/g'
    ;;
  internal/testenv/cloudprovider/*)
    # Provider GPU catalogs are spelled verbatim: "P40" is the Tesla P40
    # GpuType the compshare API expects. Two-digit NVIDIA board names are
    # external hardware vocabulary, not milestones; per arch-constraints.md
    # the exclusion is path-scoped to the package that maps provider
    # catalogs, like the udocker P1 case above.
    sed -E 's/P40/TESLA_P40/g'
    ;;
  ui/src/components/ui/*)
    # Zeron-managed components (installed by the Zeron CLI) spell SVG paths
    # inside JSX props and string constants, beyond the d="M..." form that the
    # shared filter below recognises: an "M4"/"M28" there is a path command,
    # not a milestone. Scoped to the vendored directory, like the cases above.
    sed -E 's/"M[0-9][^"]*"/"SVGPATH"/g'
    ;;
  *)
    cat
    ;;
  esac \
    | sed -E \
      -e 's/[Pp](50|75|90|95|99)/PCTL/g' \
      -e 's/d="[Mm][0-9][^"]*"/d="SVGPATH"/g' \
      -e 's|base64,[A-Za-z0-9+/=]*|base64,DATA|g'
}

process_name_hits() {
  { go_prod_files; ui_prod_files; } | while IFS= read -r f; do
    neutralize_known_tokens "$f" < "$f" \
      | grep -nE '(^|[^0-9A-Za-z_])[MSGP][0-9]{1,2}(\.[0-9])?([^0-9A-Za-z_]|$)' 2>/dev/null \
      | sed "s|^|$f:|"
  done
  go_prod_files | while IFS= read -r f; do
    b=$(basename "$f")
    if echo "$b" | grep -qE '(^|[^0-9a-z])[msgp][0-9]{1,2}([^0-9a-z]|$)'; then
      echo "$f:0:filename carries a process label"
    fi
  done
}

# soft_budget_hint prints what to do instead when a warn key is over budget.
soft_budget_hint() {
  case "$1" in
  ui_antd_icon_import_files) printf '%s' ": use ui/src/ui-kit/consoleIcons.ts instead of @ant-design/icons" ;;
  app_prod_lines) printf '%s' ": put a feature's logic in its own package; app keeps construction and adapters" ;;
  esac
}

# --- budget loop -----------------------------------------------------------
echo "architecture constraints (scripts/arch-constraints.txt):"
# A budget file whose last line lacks a trailing newline would otherwise be
# silently dropped by `read`, hiding a whole budget; the `[ -n "$line" ]` arm
# makes that final partial line count like any other.
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in
  '#'* | '') continue ;;
  esac
  key=$(printf '%s\n' "$line" | awk -F'\t' '{print $1}')
  limit=$(printf '%s\n' "$line" | awk -F'\t' '{print $2}')
  direction=$(printf '%s\n' "$line" | awk -F'\t' '{print $3}')
  if [ -z "$key" ] || [ -z "$limit" ] || [ -z "$direction" ]; then
    echo "  malformed budget line: $line" >&2
    failures=$((failures + 1))
    continue
  fi
  # A line is key, limit, direction and nothing else: a reason at the end of
  # the line is where two merge requests touching one budget conflict.
  if [ -n "$(printf '%s\n' "$line" | awk -F'\t' '{print $4}')" ]; then
    echo "  FAIL: $key carries a fourth field; reasons belong in the key's section of $BUDGET_DOC" >&2
    failures=$((failures + 1))
  fi
  if ! measured=$(measure "$key"); then
    echo "  FAIL: $key measurement failed" >&2
    exit 1
  fi
  measured=$(printf '%s' "$measured" | tr -d '[:space:]')
  if [ "$measured" = "UNKNOWN" ]; then
    echo "  FAIL: unknown budget key '$key'; add a measurement to check_architecture.sh" >&2
    failures=$((failures + 1))
    continue
  fi
  if [ "$measured" = "NOTFOUND" ]; then
    echo "  FAIL: $key: the declaration it measures was not found; move the measurement with it" >&2
    failures=$((failures + 1))
    continue
  fi
  if [ "$measured" = "UI" ]; then
    continue
  fi
  if [ "$measured" = "EXTERNAL" ]; then
    echo "  $key -/$limit ($direction; measured by scripts/check_ui_bundle_size.sh)"
    continue
  fi
  case "$measured" in
  '' | *[!0-9]*)
    echo "  FAIL: $key measurement must be a non-negative integer (got '$measured')" >&2
    exit 1
    ;;
  esac
  echo "  $key $measured/$limit ($direction)"
  if [ "$key" = "ui_antd_import_files" ] && [ "$measured" -eq 0 ]; then
    echo "WARNING: antd 已清零：请在 scripts/arch-constraints.txt 加上 ui_raw_color / ui_raw_radius 计数规则，见 docs/dev/ui.md 的迁移收尾清单"
  fi
  case "$direction" in
  max)
    if [ "$measured" -gt "$limit" ]; then
      echo "    FAIL: $key exceeds its budget ($measured > $limit)" >&2
      if [ "$key" = "ui_untested_shared_files" ]; then
        echo "      add a ui/tests/<name>.test.tsx that imports each of these, or names its path if it drives it through its host (docs/dev/ui.md, Verification):" >&2
        untested_shared_ui | sed 's/^/        /' >&2
      fi
      failures=$((failures + 1))
    elif [ "$measured" -lt "$limit" ]; then
      echo "    WARNING: $key is below its budget ($measured < $limit); budget can be tightened" >&2
      warnings=$((warnings + 1))
    fi
    ;;
  min)
    if [ "$measured" -lt "$limit" ]; then
      echo "    FAIL: $key is below its floor ($measured < $limit)" >&2
      failures=$((failures + 1))
    fi
    ;;
  warn)
    # Soft budget: reported, never failed.
    if [ "$measured" -gt "$limit" ]; then
      echo "    WARNING: $key is above its soft budget ($measured > $limit)$(soft_budget_hint "$key")" >&2
      warnings=$((warnings + 1))
    elif [ "$measured" -lt "$limit" ]; then
      echo "    WARNING: $key is below its soft budget ($measured < $limit); budget can be tightened" >&2
      warnings=$((warnings + 1))
    fi
    ;;
  *)
    echo "    FAIL: unknown direction '$direction' for $key" >&2
    failures=$((failures + 1))
    ;;
  esac
done < "$BUDGET_FILE"

# --- budget regressions vs baseline -----------------------------------------
# Part 1 above is a ratchet on THIS tree; it cannot by itself stop someone
# from raising a limit in scripts/arch-constraints.txt without a record,
# since a raised limit still "passes" its own new value. This section catches
# that by comparing the file against the copy at the merge base with
# origin/master (or, when CI names the merge request's base commit, against
# $GPUWORKS_ARCH_BASELINE; the checkout must then hold that commit). A key
# that loosened fails unless scripts/arch-constraints.md holds the line
#   - 放宽 `<key>` <old>→<new>（GWKS-<n>）：<reason>；回落条件：<condition>
# with the baseline limit as <old> and the current one as <new>. A raise that
# is already in the baseline is not looked at; a key added or removed on this
# branch is not a regression.
echo "budget regressions vs baseline (scripts/arch-constraints.txt):"

# resolve_arch_baseline prints a commit sha on stdout and returns 0, or
# returns 1 with nothing printed when no baseline is reachable. It never lets
# a failed git call escape under `set -e`: every risky command sits in an
# `if`/`||` so this function's own failure path is the only way out.
resolve_arch_baseline() {
  if [ -n "${GPUWORKS_ARCH_BASELINE:-}" ]; then
    if git cat-file -e "${GPUWORKS_ARCH_BASELINE}^{commit}" 2>/dev/null; then
      printf '%s\n' "$GPUWORKS_ARCH_BASELINE"
      return 0
    fi
  fi
  if git rev-parse --verify -q origin/master >/dev/null 2>&1; then
    base=$(git merge-base HEAD origin/master 2>/dev/null) || return 1
    if [ -n "$base" ]; then
      printf '%s\n' "$base"
      return 0
    fi
  fi
  return 1
}

baseline_sha=$(resolve_arch_baseline || true)
if [ -z "$baseline_sha" ]; then
  echo "  NOTE: no baseline commit for $BUDGET_FILE (shallow clone, no origin/master, or nothing to diff against); skipping"
elif [ "$baseline_sha" = "$(git rev-parse HEAD)" ]; then
  echo "  NOTE: HEAD is the baseline commit; nothing to compare for $BUDGET_FILE"
else
  baseline_tmp=$(mktemp)
  if git show "$baseline_sha:$BUDGET_FILE" >"$baseline_tmp" 2>/dev/null; then
    section_failures=0
    # Reading from a file (not a pipe) keeps this loop out of a subshell, so
    # `failures`/`section_failures` updated inside it are still visible below.
    while IFS= read -r bline || [ -n "$bline" ]; do
      case "$bline" in
      '#'* | '') continue ;;
      esac
      bkey=$(printf '%s\n' "$bline" | awk -F'\t' '{print $1}')
      blimit=$(printf '%s\n' "$bline" | awk -F'\t' '{print $2}')
      bdirection=$(printf '%s\n' "$bline" | awk -F'\t' '{print $3}')
      [ -z "$bkey" ] && continue

      # Current line for the same key, found by field 1 rather than a
      # tab-embedded grep pattern (fragile to reproduce in a shell literal).
      cline=$(awk -F'\t' -v k="$bkey" '$1 == k {print; exit}' "$BUDGET_FILE")
      if [ -z "$cline" ]; then
        continue # key removed on this branch; not a regression.
      fi
      climit=$(printf '%s\n' "$cline" | awk -F'\t' '{print $2}')
      cdirection=$(printf '%s\n' "$cline" | awk -F'\t' '{print $3}')
      if [ "$cdirection" != "$bdirection" ]; then
        continue # a direction flip is caught by the per-key loop above.
      fi
      loosened=no
      case "$cdirection" in
      max | warn) [ "$climit" -gt "$blimit" ] && loosened=yes ;;
      min) [ "$climit" -lt "$blimit" ] && loosened=yes ;;
      esac
      record="- 放宽 \`$bkey\` $blimit→$climit（GWKS-"
      if [ "$loosened" = yes ] \
        && ! grep -E "^$record[0-9]+）：.+；回落条件：.+" "$BUDGET_DOC" >/dev/null 2>&1; then
        echo "  FAIL: $bkey $cdirection budget loosened ($blimit -> $climit) without a record; add this line under its section in $BUDGET_DOC:" >&2
        echo "    $record<number>）：<reason>；回落条件：<condition for bringing it back to $blimit>" >&2
        failures=$((failures + 1))
        section_failures=$((section_failures + 1))
      fi
    done <"$baseline_tmp"
    if [ "$section_failures" -eq 0 ]; then
      echo "  ok: no budget loosened without a record in $BUDGET_DOC (baseline $baseline_sha)"
    fi
  else
    echo "  NOTE: $BUDGET_FILE not found at baseline $baseline_sha; skipping"
  fi
  rm -f "$baseline_tmp"
fi

# --- dependency direction (plan §5.3) --------------------------------------
# Hard rules, never budgets: a violation here is an inverted layer, not debt.
echo "dependency direction:"

pkg_imports() {
  go list -e -f '{{join .Imports "\n"}}' "$1" 2>/dev/null || true
}

forbid_import() {
  # forbid_import <package pattern> <forbidden import substring> <label>
  hit=$(pkg_imports "$1" | grep -F "$2" || true)
  if [ -n "$hit" ]; then
    echo "  FAIL: $3" >&2
    echo "$hit" | sed 's/^/    /' >&2
    failures=$((failures + 1))
    return 0
  fi
  echo "  ok: $3"
}

# Leaf packages named by the plan (§2.3) must not depend on store. Two of them
# do today and are scheduled to be cut in the store-boundary phase; they are
# waived by name so the rule still catches a *new* leaf reaching for store. A
# waiver that no longer applies is itself a failure — delete it when the phase
# lands, and the check becomes unconditional again.
#
# Space-delimited on both ends so a `case` glob matches whole names only; `case`
# avoids relying on unquoted word splitting, which zsh does not do.
LEAF_STORE_WAIVED=' inferenceapi partitioncap '

leaf_is_waived() {
  case "$LEAF_STORE_WAIVED" in
  *" $1 "*) return 0 ;;
  esac
  return 1
}

for leaf in inferenceapi messages placement acquisition chipmatch partitioncap netplan pdmode kvbackend nodelabel uxrecord uxstore tracecontext traceexport usagereport yieldpolicy lifecycle; do
  if pkg_imports "./internal/server/$leaf" | grep -qF 'gpuworks/internal/server/store'; then
    if leaf_is_waived "$leaf"; then
      echo "  WAIVED: leaf package $leaf still imports store (planned removal)"
      waivers=$((waivers + 1))
    else
      echo "  FAIL: leaf package $leaf imports store" >&2
      failures=$((failures + 1))
    fi
  elif leaf_is_waived "$leaf"; then
    echo "  FAIL: leaf package $leaf no longer imports store; drop it from LEAF_STORE_WAIVED" >&2
    failures=$((failures + 1))
  else
    echo "  ok: leaf package $leaf does not import store"
  fi
done

forbid_import ./internal/server/api \
  'gpuworks/internal/server/controller' \
  'api does not import controller'

# The deployment use cases take a request type and answer a plan, a row or a
# ProductError; the HTTP handlers map that onto a status. They know neither
# HTTP nor the handlers, so automation, batches and MCP can call them too.
# Bespoke rather than forbid_import: forbid_import matches a substring, so a
# rule against .../server/api would also refuse api/dto, the request and
# response types the use cases take and answer.
usecase_imports=$(pkg_imports ./internal/server/deployment)
usecase_http=$(printf '%s\n' "$usecase_imports" | grep -xE 'net/http|gpuworks\.ai/gpuworks/internal/server/(api|hub|controller|proxy)' || true)
if [ -z "$usecase_imports" ]; then
  echo "  FAIL: deployment has no imports to check (package missing or broken)" >&2
  failures=$((failures + 1))
elif [ -n "$usecase_http" ]; then
  echo "  FAIL: deployment imports none of net/http, api, hub, controller, proxy" >&2
  echo "$usecase_http" | sed 's/^/    /' >&2
  failures=$((failures + 1))
else
  echo "  ok: deployment imports none of net/http, api, hub, controller, proxy"
fi

# Network decisions live in netplan (pure) and the network service; the store
# only keeps the rows, so it must not reach back for either.
forbid_import ./internal/server/store \
  'gpuworks/internal/server/netplan' \
  'store does not import netplan'
forbid_import ./internal/server/store \
  'gpuworks/internal/server/network' \
  'store does not import network'
netplan_deps=$(pkg_imports ./internal/server/netplan | grep 'gpuworks/' | grep -vx 'gpuworks.ai/gpuworks/internal/proto' || true)
if [ -n "$netplan_deps" ]; then
  echo "  FAIL: netplan depends only on proto" >&2
  echo "$netplan_deps" | sed 's/^/    /' >&2
  failures=$((failures + 1))
else
  echo "  ok: netplan depends only on proto"
fi
# Metrics and log ingestion writes its own stores and knows nothing of the
# server around it: no store, api, hub, planning or app (GWKS-307). The app
# adapts the stores to those.
obsingest_deps=$(pkg_imports ./internal/server/obsingest | grep 'gpuworks/' \
  | grep -vxE 'gpuworks\.ai/gpuworks/internal/(proto|selflog|server/(logstore|tsdb|uxstore))' || true)
if [ -z "$(pkg_imports ./internal/server/obsingest)" ]; then
  echo "  FAIL: obsingest has no imports to check (package missing or broken)" >&2
  failures=$((failures + 1))
elif [ -n "$obsingest_deps" ]; then
  echo "  FAIL: obsingest depends only on proto, selflog, logstore, tsdb and uxstore" >&2
  echo "$obsingest_deps" | sed 's/^/    /' >&2
  failures=$((failures + 1))
else
  echo "  ok: obsingest depends only on proto, selflog, logstore, tsdb and uxstore"
fi
# The feature gate is read on every extension request and by the extensions a
# separately built server adds: it answers availability from a snapshot and
# builds its refusals, so it needs messages and nothing else of this tree.
featuregate_deps=$(pkg_imports ./internal/server/featuregate | grep 'gpuworks/' \
  | grep -vxE 'gpuworks\.ai/gpuworks/internal/server/messages' || true)
if [ -z "$(pkg_imports ./internal/server/featuregate)" ]; then
  echo "  FAIL: featuregate has no imports to check (package missing or broken)" >&2
  failures=$((failures + 1))
elif [ -n "$featuregate_deps" ]; then
  echo "  FAIL: featuregate depends only on messages" >&2
  echo "$featuregate_deps" | sed 's/^/    /' >&2
  failures=$((failures + 1))
else
  echo "  ok: featuregate depends only on messages"
fi
# Site rule grammar is a pure leaf beside pullref: store and planning share it.
sourceurl_deps=$(pkg_imports ./internal/server/sourceurl | grep 'gpuworks/' || true)
if [ -n "$sourceurl_deps" ]; then
  echo "  FAIL: sourceurl imports nothing from this repository" >&2
  echo "$sourceurl_deps" | sed 's/^/    /' >&2
  failures=$((failures + 1))
else
  echo "  ok: sourceurl imports nothing from this repository"
fi
# A model download walks its addresses in modelfetch; the agent's task
# lifecycle and the registry prober stay outside it (the prober is reached
# through modelfetch.Cooler).
modelfetch_deps=$(pkg_imports ./internal/agent/modelfetch | grep -E 'gpuworks/internal/agent/(app|registryprobe)$' || true)
if [ -n "$modelfetch_deps" ]; then
  echo "  FAIL: modelfetch does not depend on agent/app or registryprobe" >&2
  echo "$modelfetch_deps" | sed 's/^/    /' >&2
  failures=$((failures + 1))
else
  echo "  ok: modelfetch does not depend on agent/app or registryprobe"
fi
# The distribution client is shared by the install/upgrade CLI and the API;
# selfinstall must not grow a dependency on the rest of the server through it.
distribution_deps=$(pkg_imports ./internal/server/distribution | grep 'gpuworks/' || true)
if [ -n "$distribution_deps" ]; then
  echo "  FAIL: distribution imports nothing from this repository" >&2
  echo "$distribution_deps" | sed 's/^/    /' >&2
  failures=$((failures + 1))
else
  echo "  ok: distribution imports nothing from this repository"
fi
forbid_import ./internal/server/controller \
  'gpuworks/internal/server/api' \
  'controller does not import api'

# Shared orchestration only sees provider contracts. The app imports all solely
# to register drivers; concrete assembly and ownership rules belong to drivers.
# The connection-test runner (provider/testconn) is a leaf the service drives,
# not a driver, so it is exempt from the driver-import ban.
provider_driver_imports=$(pkg_imports ./internal/server/provider \
  | grep -F 'gpuworks/internal/server/provider/' \
  | grep -vx 'gpuworks.ai/gpuworks/internal/server/provider/testconn' || true)
if [ -n "$provider_driver_imports" ]; then
  echo "  FAIL: provider service does not import concrete drivers" >&2
  echo "$provider_driver_imports" | sed 's/^/    /' >&2
  failures=$((failures + 1))
else
  echo "  ok: provider service does not import concrete drivers"
fi
# The exemption holds only while testconn stays a leaf: it knows neither the
# provider contracts (the service hands it plain inputs and vendor checks),
# nor any driver, nor the store.
forbid_import ./internal/server/provider/testconn \
  'gpuworks/internal/server/provider' \
  'provider connection-test runner does not import the provider service or drivers'
forbid_import ./internal/server/provider/testconn \
  'gpuworks/internal/server/store' \
  'provider connection-test runner does not import store'
forbid_import ./internal/server/controller \
  'gpuworks/internal/server/provider/' \
  'controller does not import concrete provider drivers'
app_provider_deps=$(pkg_imports ./internal/server/app \
  | grep '^gpuworks.ai/gpuworks/internal/server/provider/' \
  | grep -vx 'gpuworks.ai/gpuworks/internal/server/provider/all' || true)
if [ -n "$app_provider_deps" ]; then
  echo "  FAIL: app uses provider registration rather than concrete drivers" >&2
  echo "$app_provider_deps" | sed 's/^/    /' >&2
  failures=$((failures + 1))
else
  echo "  ok: app uses provider registration rather than concrete drivers"
fi

tree_forbids() {
  # tree_forbids <package tree> <forbidden import substring> <label>
  # Only the import list is matched, never the importing package's own path —
  # otherwise ./internal/proto trivially "matches" gpuworks/ via its own name.
  hit=$(go list -e -f '{{.ImportPath}}	{{join .Imports " "}}' "$1" 2>/dev/null \
    | awk -F'\t' -v needle="$2" 'index($2, needle) {print $1}' | sort -u)
  if [ -n "$hit" ]; then
    echo "  FAIL: $3" >&2
    echo "$hit" | sed 's/^/    /' >&2
    failures=$((failures + 1))
    return 0
  fi
  echo "  ok: $3"
}

tree_forbids ./pkg/... 'gpuworks/internal' 'pkg does not import internal'

# kvbackend is a pure data table (ADR 0069): its only in-repo imports are the
# vendor, engine and PD role vocabularies it names. Rendering and gating live
# in the engine adapter (internal/server/deploy).
kvbackend_extra=$(go list -e -f '{{join .Imports "\n"}}' ./internal/server/kvbackend 2>/dev/null \
  | grep '^gpuworks\.ai/gpuworks/' \
  | grep -vxE 'gpuworks\.ai/gpuworks/internal/(accelerator|engines|server/pdmode)' || true)
if [ -n "$kvbackend_extra" ]; then
  echo "  FAIL: kvbackend imports only accelerator, engines and pdmode" >&2
  echo "$kvbackend_extra" | sed 's/^/    /' >&2
  failures=$((failures + 1))
else
  echo "  ok: kvbackend imports only accelerator, engines and pdmode"
fi
tree_forbids ./internal/agent/... 'gpuworks/internal/server' 'agent does not import server'
tree_forbids ./internal/proto 'gpuworks/' 'proto has no in-repo imports'
tree_forbids ./internal/platformmetrics 'gpuworks/' 'platformmetrics has no in-repo imports (agent and server share it)'
tree_forbids ./internal/launchd 'gpuworks/' 'launchd has no in-repo imports (agent and server share it)'
tree_forbids ./internal/hwident 'gpuworks/' 'hwident has no in-repo imports (agent and server share it)'
tree_forbids ./internal/agent/connbudget 'gpuworks/' 'connbudget imports nothing from this repository (a leaf: image pulls and model downloads share it)'
tree_forbids ./internal/server/mcp 'gpuworks/' 'mcp speaks the protocol only: it knows neither callers nor tools'
tree_forbids ./internal/agent/gpu/cardsmoke 'gpuworks/' 'cardsmoke schedules attempts and knows no vendor'
tree_forbids ./internal/agent/gpu/pcicards 'gpuworks/internal/agent' 'pcicards reads sysfs and knows nothing of the agent'
tree_forbids ./internal/server/slurmboot 'gpuworks/' 'slurmboot imports nothing from this repository (a leaf: parameters in, script text out)'

# --- public wire module ------------------------------------------------------
# pkg/extapi is a module of its own: the wire formats plugins import without
# the server. `go list ./...` at the root does not enter it, so its rules are
# checked here: standard library only (tests included), no requirement, the
# root's go line, and a golden corpus whose released files are never edited
# or removed (SHA256SUMS only grows against the baseline commit).
echo "public wire module (pkg/extapi):"
EXTAPI_DIR=pkg/extapi
EXTAPI_MOD=gpuworks.ai/gpuworks/pkg/extapi
if [ ! -f "$EXTAPI_DIR/go.mod" ]; then
  echo "  FAIL: $EXTAPI_DIR/go.mod is missing" >&2
  failures=$((failures + 1))
else
  if ! extapi_deps=$(cd "$EXTAPI_DIR" && go list -e -deps -test \
    -f '{{if not .Standard}}{{.ImportPath}}{{if .Error}} ERROR{{end}}{{end}}' ./... 2>&1); then
    extapi_deps="go list failed: $extapi_deps"
  fi
  extapi_foreign=$(printf '%s\n' "$extapi_deps" | grep -v '^$' \
    | grep -vE "^${EXTAPI_MOD}( \[${EXTAPI_MOD}(\.test)?\]|\.test|_test \[${EXTAPI_MOD}\.test\])?\$" || true)
  if [ -n "$extapi_foreign" ]; then
    echo "  FAIL: pkg/extapi imports only the standard library" >&2
    echo "$extapi_foreign" | sed 's/^/    /' >&2
    failures=$((failures + 1))
  else
    echo "  ok: pkg/extapi imports only the standard library"
  fi
  extapi_require=$(grep -nE '^[[:space:]]*(require|replace)([[:space:]]|\()' "$EXTAPI_DIR/go.mod" || true)
  root_go=$(awk '$1 == "go" {print $2; exit}' go.mod)
  extapi_go=$(awk '$1 == "go" {print $2; exit}' "$EXTAPI_DIR/go.mod")
  if [ -n "$extapi_require" ] || [ "$root_go" != "$extapi_go" ]; then
    echo "  FAIL: pkg/extapi/go.mod requires nothing and has the root's go line ($root_go, has $extapi_go)" >&2
    failures=$((failures + 1))
  else
    echo "  ok: pkg/extapi/go.mod requires nothing and has the root's go line"
  fi
  if ! grep -qE "^replace[[:space:]]+${EXTAPI_MOD}[[:space:]]+=>[[:space:]]+\./${EXTAPI_DIR}\$" go.mod; then
    echo "  FAIL: go.mod replaces $EXTAPI_MOD with ./$EXTAPI_DIR" >&2
    failures=$((failures + 1))
  else
    echo "  ok: go.mod builds against ./$EXTAPI_DIR"
  fi
  golden_sums="$EXTAPI_DIR/testdata/golden/SHA256SUMS"
  if [ -n "${baseline_sha:-}" ] && [ "$baseline_sha" != "$(git rev-parse HEAD 2>/dev/null)" ] \
    && git cat-file -e "$baseline_sha:$golden_sums" 2>/dev/null; then
    golden_lost=$(git show "$baseline_sha:$golden_sums" | while IFS= read -r line; do
      grep -qxF -- "$line" "$golden_sums" 2>/dev/null || printf '%s\n' "$line"
    done)
    if [ -n "$golden_lost" ]; then
      echo "  FAIL: released wire goldens are never edited or removed (add a new version instead)" >&2
      echo "$golden_lost" | sed 's/^/    /' >&2
      failures=$((failures + 1))
    else
      echo "  ok: released wire goldens unchanged (baseline $baseline_sha)"
    fi
  else
    echo "  NOTE: no baseline golden list to compare; skipping the append-only check"
  fi
fi

# --- plugin SDK module -------------------------------------------------------
# pkg/pluginsdk is what plugins are built with, a module of its own beside
# pkg/extapi: a plugin imports these two and nothing of the server, so the
# SDK imports only the standard library and pkg/extapi (tests included), and
# its go.mod requires only pkg/extapi, from the directory beside it. The fake
# plugin the contract tests build stands for a third-party plugin and is held
# to the same rule.
echo "plugin SDK module (pkg/pluginsdk):"
SDK_DIR=pkg/pluginsdk
SDK_MOD=gpuworks.ai/gpuworks/pkg/pluginsdk
if [ ! -f "$SDK_DIR/go.mod" ]; then
  echo "  FAIL: $SDK_DIR/go.mod is missing" >&2
  failures=$((failures + 1))
else
  if ! sdk_deps=$(cd "$SDK_DIR" && go list -e -deps -test \
    -f '{{if not .Standard}}{{.ImportPath}}{{if .Error}} ERROR{{end}}{{end}}' ./... 2>&1); then
    sdk_deps="go list failed: $sdk_deps"
  fi
  sdk_foreign=$(printf '%s\n' "$sdk_deps" | grep -v '^$' \
    | grep -vE "^(${SDK_MOD}(/[a-z0-9/]+)?|${EXTAPI_MOD})( \[${SDK_MOD}(/[a-z0-9/]+)?(\.test)?\]|\.test|_test \[${SDK_MOD}(/[a-z0-9/]+)?\.test\])?\$" || true)
  if [ -n "$sdk_foreign" ]; then
    echo "  FAIL: pkg/pluginsdk imports only the standard library and pkg/extapi" >&2
    echo "$sdk_foreign" | sed 's/^/    /' >&2
    failures=$((failures + 1))
  else
    echo "  ok: pkg/pluginsdk imports only the standard library and pkg/extapi"
  fi
  sdk_require=$(grep -E '^[[:space:]]*(require|replace)([[:space:]]|\()' "$SDK_DIR/go.mod" | sed 's/[[:space:]]\+/ /g' || true)
  sdk_want="require ${EXTAPI_MOD} v0.0.0
replace ${EXTAPI_MOD} => ../extapi"
  sdk_go=$(awk '$1 == "go" {print $2; exit}' "$SDK_DIR/go.mod")
  if [ "$sdk_require" != "$sdk_want" ] || [ "$(awk '$1 == "go" {print $2; exit}' go.mod)" != "$sdk_go" ]; then
    echo "  FAIL: pkg/pluginsdk/go.mod requires only pkg/extapi from ../extapi and has the root's go line" >&2
    failures=$((failures + 1))
  else
    echo "  ok: pkg/pluginsdk/go.mod requires only pkg/extapi from ../extapi"
  fi
  if ! grep -qE "^replace[[:space:]]+${SDK_MOD}[[:space:]]+=>[[:space:]]+\./${SDK_DIR}\$" go.mod; then
    echo "  FAIL: go.mod replaces $SDK_MOD with ./$SDK_DIR" >&2
    failures=$((failures + 1))
  else
    echo "  ok: go.mod builds against ./$SDK_DIR"
  fi
fi
FAKE_PLUGIN=./internal/testenv/extplugin/fakeplugin
if [ -d "$FAKE_PLUGIN" ]; then
  fake_foreign=$(go list -e -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' "$FAKE_PLUGIN" 2>&1 \
    | grep -v '^$' | grep -vE "^(${SDK_MOD}(/[a-z0-9/]+)?|${EXTAPI_MOD}|gpuworks\.ai/gpuworks/internal/testenv/extplugin/fakeplugin)\$" || true)
  if [ -n "$fake_foreign" ]; then
    echo "  FAIL: the fake plugin imports only pkg/pluginsdk, pkg/extapi and the standard library, as a plugin does" >&2
    echo "$fake_foreign" | sed 's/^/    /' >&2
    failures=$((failures + 1))
  else
    echo "  ok: the fake plugin imports only pkg/pluginsdk, pkg/extapi and the standard library"
  fi
fi

# --- console overlays --------------------------------------------------------
# Console overlay modules (ui/src/extensions/<id>/) belong to the separately
# built console: they are copied in, git-ignored, and left out of the UI
# budgets, which is only sound while none of them is committed here.
echo "console overlays:"
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  tracked_overlays=$(git ls-files -- 'ui/src/extensions/*/*' 2>/dev/null || true)
  if [ -n "$tracked_overlays" ]; then
    echo "  FAIL: no tracked file under ui/src/extensions/<id>/ (console overlays are copied in, not committed)" >&2
    echo "$tracked_overlays" | sed 's/^/    /' >&2
    failures=$((failures + 1))
  else
    echo "  ok: no tracked console overlay under ui/src/extensions/"
  fi
fi

# --- verdict ---------------------------------------------------------------
if [ "$failures" -gt 0 ]; then
  echo "architecture check failed: $failures violation(s)" >&2
  exit 1
fi
summary="architecture check passed"
if [ "$waivers" -gt 0 ]; then
  summary="$summary; $waivers waived layer violation(s)"
fi
if [ "$warnings" -gt 0 ]; then
  summary="$summary; $warnings slack warning(s), tighten scripts/arch-constraints.txt"
fi
echo "$summary"
