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
#      origin/master. A "max" key whose limit grew, or a "min" key whose limit
#      shrank, fails unless that line carries a trailing `# raised: <reason>`
#      comment. New or removed keys are not a regression. When no baseline can
#      be resolved (shallow clone, no origin remote, already on the default
#      branch) this prints a NOTE and is skipped, never failed.
#      A third direction, "warn", is a soft budget: over the limit prints a
#      WARNING and never fails, and the baseline comparison ignores it.
#      ui_bundle_gzip_kb is a warn key measured by scripts/check_ui_bundle_size.sh
#      after a UI build; this script only skips it.
#   3. Dependency direction — hard structural rules (plan §5.3). These are not
#      budgets: any violation fails immediately.
#
# Intent, exact command and exclusion list per key: scripts/arch-constraints.md

cd "$(dirname "$0")/.."

BUDGET_FILE="scripts/arch-constraints.txt"
if [ ! -f "$BUDGET_FILE" ]; then
  echo "missing $BUDGET_FILE" >&2
  exit 1
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

# Production UI sources: ui/src minus *.test.* (and minus the i18n locale JSON,
# which is translated text rather than code).
ui_prod_files() {
  find ui/src -type f \( -name '*.ts' -o -name '*.tsx' -o -name '*.css' \) \
    ! -name '*.test.ts' ! -name '*.test.tsx' | sort
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
      | grep -v '\.test\.' | grep -v '^ui/src/i18n/' | grep -c . || true
    ;;
  ui_antd_import_files)
    # Count source files, including re-exports, dynamic imports and subpaths.
    grep -rlE "['\"]antd(/[^'\"]*)?['\"]" ui/src \
      --include='*.ts' --include='*.tsx' 2>/dev/null \
      | grep -v '\.test\.' | grep -c . || true
    ;;
  ui_antd_icon_import_files)
    grep -rlE "['\"]@ant-design/icons(/[^'\"]*)?['\"]" ui/src \
      --include='*.ts' --include='*.tsx' 2>/dev/null \
      | grep -v '\.test\.' | grep -v '^ui/src/components/ui/' | grep -c . || true
    ;;
  ui_untested_shared_files)
    untested_shared_ui | grep -c . || true
    ;;
  ui_copy_long_strings)
    node scripts/ui_copy_audit.mjs long
    ;;
  ui_copy_caveat_strings)
    node scripts/ui_copy_audit.mjs caveat
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
  infer_vendor_callers)
    grep -rn 'deploy\.InferVendor(' internal pkg cmd --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' | grep -c . || true
    ;;
  store_concrete_importers)
    go_imports \
      | grep -v '\.test$' \
      | awk -F'\t' '$2 ~ /gpuworks\/internal\/server\/store( |$)/ {print $1}' \
      | sort -u | grep -c . || true
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
  *)
    # `measure` runs inside a command substitution, so exiting here would only
    # kill the subshell. Emit a sentinel the caller turns into a failure.
    echo "UNKNOWN"
    ;;
  esac
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
  measured=$(measure "$key" | tr -d '[:space:]')
  if [ "$measured" = "UNKNOWN" ]; then
    echo "  FAIL: unknown budget key '$key'; add a measurement to check_architecture.sh" >&2
    failures=$((failures + 1))
    continue
  fi
  if [ "$measured" = "EXTERNAL" ]; then
    echo "  $key -/$limit ($direction; measured by scripts/check_ui_bundle_size.sh)"
    continue
  fi
  [ -n "$measured" ] || measured=0
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

# --- antd per-file ratchet -------------------------------------------------
# ui_antd_import_files only counts files; ui/antd-inventory.json records each
# file's antd usage (sites, imported names, icons, .ant-* rules) and the
# inventory fails when any file grew or a new file started using antd.
# Shrinking passes; the snapshot is refreshed with --write in the same MR.
echo "antd per-file ratchet (ui/antd-inventory.json):"
if command -v node >/dev/null 2>&1; then
  if inventory_out=$(node scripts/ui_antd_inventory.mjs --check ui/antd-inventory.json 2>&1); then
    printf '%s\n' "$inventory_out" | sed 's/^/  /'
  else
    printf '%s\n' "$inventory_out" | sed 's/^/  /' >&2
    echo "  FAIL: antd usage grew against ui/antd-inventory.json" >&2
    failures=$((failures + 1))
  fi
else
  echo "  NOTE: node not found; skipping the antd per-file ratchet"
fi

# --- budget regressions vs baseline -----------------------------------------
# Part 1 above is a ratchet on THIS tree; it cannot by itself stop someone
# from raising a limit in scripts/arch-constraints.txt without saying why,
# since a raised limit still "passes" its own new value. This section catches
# that by comparing the file against the copy at the merge base with
# origin/master (or, when CI names the merge request's base commit, against
# $GPUWORKS_ARCH_BASELINE; the checkout must then hold that commit). A key that loosened without a trailing
# `# raised: <reason>` comment fails; a key added or removed on this branch is
# not a regression.
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
      raised=no
      case "$cline" in
      *'# raised:'*) raised=yes ;;
      esac

      case "$cdirection" in
      max)
        if [ "$climit" -gt "$blimit" ] && [ "$raised" != yes ]; then
          echo "  FAIL: $bkey max budget loosened without a '# raised: <reason>' comment ($blimit -> $climit)" >&2
          failures=$((failures + 1))
          section_failures=$((section_failures + 1))
        fi
        ;;
      min)
        if [ "$climit" -lt "$blimit" ] && [ "$raised" != yes ]; then
          echo "  FAIL: $bkey min budget loosened without a '# raised: <reason>' comment ($blimit -> $climit)" >&2
          failures=$((failures + 1))
          section_failures=$((section_failures + 1))
        fi
        ;;
      esac
    done <"$baseline_tmp"
    if [ "$section_failures" -eq 0 ]; then
      echo "  ok: no budget loosened without a recorded reason (baseline $baseline_sha)"
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

for leaf in inferenceapi messages placement acquisition chipmatch partitioncap netplan pdmode nodelabel uxrecord uxstore tracecontext traceexport usagereport yieldpolicy; do
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
forbid_import ./internal/server/provider \
  'gpuworks/internal/server/provider/' \
  'provider service does not import concrete drivers'
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
tree_forbids ./internal/agent/... 'gpuworks/internal/server' 'agent does not import server'
tree_forbids ./internal/proto 'gpuworks/' 'proto has no in-repo imports'
tree_forbids ./internal/platformmetrics 'gpuworks/' 'platformmetrics has no in-repo imports (agent and server share it)'
tree_forbids ./internal/launchd 'gpuworks/' 'launchd has no in-repo imports (agent and server share it)'
tree_forbids ./internal/agent/connbudget 'gpuworks/' 'connbudget imports nothing from this repository (a leaf: image pulls and model downloads share it)'
tree_forbids ./internal/server/mcp 'gpuworks/' 'mcp speaks the protocol only: it knows neither callers nor tools'
tree_forbids ./internal/agent/gpu/cardsmoke 'gpuworks/' 'cardsmoke schedules attempts and knows no vendor'
tree_forbids ./internal/agent/gpu/pcicards 'gpuworks/internal/agent' 'pcicards reads sysfs and knows nothing of the agent'

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
