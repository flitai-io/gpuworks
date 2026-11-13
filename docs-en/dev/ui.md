# UI development

Vite builds `ui/` into `internal/server/webui/dist`, embedded in the Go server.
The console uses React 19, Tailwind 4 and the Zeron design system alongside
antd 5 while pages migrate one by one. The shell, form layer, shared components
(page header, resource table, detail drawer), `/batches` and `/dashboard` are on
Zeron; other pages still render antd inside them. New code uses Zeron and
`ui-kit/`. Preserve APIs, permissions, routes and server-owned rules.

## Directories and public APIs

- `ui/src/components/ui/`, managed `lib/` and `hooks/`: CLI-owned Zeron sources;
  provenance and transformed hashes live in `ui/.zeron/install-state.json`.
- `ui/src/ui-kit/`: project compositions: SideDrawer, NumberInput, FileUpload,
  ConfirmPopover, Spinner, Progress, Segmented, Timeline and Result.
- `ui-kit/form/`: react-hook-form controllers connected to Zeron Field; caller
  schemas use zod and server dictionary limits. `formSubmit` focuses and scrolls
  to the first error. Set `shouldFocusError: false` to use this ordering.
- `ui-kit/feedback/`: imperative message/confirm bridge and a single FeedbackHost.
  Migrate callers deliberately; this is not the entire antd imperative API.
- `components/PageHeader`, `ResourceTable`, `detail/DetailDrawer`, `EmptyState`:
  shared page structure. ResourceTable uses Zeron Table and useDataTable because
  the pinned DataTable renderer lacks row props and resizable header slots.
  Its temporary antd **types** adapt existing callers; query and selection state
  remain owned by callers. Do not add unsupported legacy options silently.
- `ui/src/pages/`: business compositions; `theme/`: Zeron-derived antd/ECharts
  values; `i18n/locales/{zh,en}/`: text, with `ui/i18n/glossary.json` terminology.

## Pinned source and synchronization

Zeron source: https://github.com/Carlosfengv/zeron-ui at
`1e036c58957a752bbd3ed61866f4616118a7dc28` (CLI 0.2.0-beta.18). The pin lives
only in `ui/src/theme/zeron/source.json` (`commit`, `repository`).
Do not modify managed sources. Product adaptations belong in `ui-kit/` or pages.

Install a component (git, Node >= 20.18.1 and npm needed; first run fetches):

```bash
scripts/zeron-registry.sh add <names...> --dry-run   # preview files and dependencies
scripts/zeron-registry.sh add <names...>             # install into ui/
scripts/zeron-registry.sh check                      # CLI doctor + managed file hashes
```

The script fetches the pinned commit into
`${XDG_CACHE_HOME:-~/.cache}/gpuworks/zeron/<commit>` (git's own
`http.https://github.com.proxy` is honoured; `ZERON_GIT_URL=<clone>` fetches
from an existing clone), verifies the commit, installs the CLI's dependencies,
and builds the Registry from that revision's committed `public/r` with
`registryDependencies` URLs rebased to `http://127.0.0.1:5211/r` and a
`manifest.json` of source and rebased hashes. `add` and `check` run a temporary
server on 5211 (or reuse one already serving the same revision) and stop it on
exit; `scripts/zeron-registry.sh serve` keeps one running. Keep port 5211:
install-state records the Registry base URL.

1. Check out the approved revision separately and read its swap-to-zeronui and
   zeron-page-builder skills. Review dependency closure and public APIs.
2. Install with `scripts/zeron-registry.sh add <names>` (dry run first).
   Resolve conflicts before installation; retain install-state provenance.
   Upgrading Zeron means a new pin in `source.json`, a reinstall and step 3,
   not editing managed files.
3. The CLI owns static `:root` and `.dark` tokens in `ui/src/app.css`. Run
   `node scripts/generate-zeron-tokens.mjs` to regenerate only TypeScript
   integration values. Never import upstream `tokens.css` or use `light-dark()`.
4. Run the theme consistency test (`ui/tests/zeronTheme.test.ts`) and the checks below.

Chrome 121 / Firefox 123 remain the browser floor. Verify the pinned production
CSS in both; do not infer compatibility from a modern browser. Hidden dark
preview sets both the HTML theme attribute and `dark` class. Charts already on
ECharts use Zeron chart-1…7 colors until they move to Zeron charts (see
"Choosing a component"). antd icons use `ui-kit/antdIcon` for Zeron icon slots.

## Verification

Per change, in `ui/`: `npx tsc -b`, `npx eslint .`, `npm run build` and the
related unit tests; from the root: `node scripts/i18n-check.mjs` and
`./scripts/check_architecture.sh`. CI runs the full suite.

**Unit tests** (`ui/tests/*.test.ts(x)`, React Testing Library under jsdom; run
by `npm test` in `ui/`, `make test` and CI's `core-ui-test` on every pipeline).
Each shared building block has its own test file: everything in `ui-kit/`,
`app/`, `components/detail/`, `components/resourceTable/`, and `PageHeader` /
`EmptyState`. The test imports it, or names its `ui/src/...` path when it drives
it through its real host (the sidebar through `App`). A page test that happens
to render one does not count; `ui_untested_shared_files` in `scripts/arch-constraints.txt` (max 0)
fails the architecture check and names any that has none. Change a building
block and its test in the same commit; add one with its test.

A test states behaviour the way a user meets it: by visible text, role,
accessible name and keyboard (Tab, Enter, Escape, focus returning to the
opener), and covers loading, empty, error and success. Do not snapshot, mock
Zeron or Base UI internals, or wait on fixed sleeps. Measure with
`npm run test:coverage` (`c8 --all`); a shared building block aims at 90% of
lines and 80% of branches, and a branch left untested is one the UI cannot
reach. Sizes, spacing, colours and overlay stacking are not unit-tested: jsdom
does no layout. The browser check below owns them.

When a test changes its expectation, the commit says why; a reviewer checks
that the behaviour change was intended rather than a test bent to pass.

**antd inventory** (`scripts/ui_antd_inventory.mjs`). How much antd is left, per
component, file and menu page; `ui/antd-inventory.json` is the committed
snapshot. `--check` (in CI's core-ui) fails when any file uses more antd than
the snapshot records, or a new file starts using it; after migrating, refresh
the snapshot with `--write` and lower `ui_antd_import_files` in
`scripts/arch-constraints.txt` in the same change. `--affected <files>` /
`--base <ref>` names the pages a change reaches (`["*"]` for shared files).

**Browser check** (`ui/tests/browser/ui-check.mjs`, run by
`scripts/ui_check_ci.sh` and the `ui-check` CI job). Walks a disposable
experience environment: every menu page, the first row's detail drawer and each
of its tabs, and each dialog the page header opens, at 1440 and 1024 in zh and
en. It fails on covered or overlapping text, controls stretched past their
content, controls an invisible layer blocks, sideways scrolling and console
errors. On a merge request it walks only the pages the change reaches, at 1024
only, and photographs the target branch's UI for the states that look different; the
report (`ui-check-report/index.html`, a job artifact) is for a reviewer to
confirm intended changes. Locally:

```sh
scripts/ui_check_ci.sh                                    # every page
UI_CHECK_BASE_REF=origin/master UI_CHECK_COMPARE=1 scripts/ui_check_ci.sh
```

It needs Go, Node and Chrome (or `CHROME_PATH`), rebuilds the UI when sources
are newer than the build, and refuses to start when its ports (8899, 8900) are
taken. A new page, drawer, tab or header dialog is covered without registering
it; flows behind other buttons (row menus, multi-step forms) are not walked
yet and still need a person or a dedicated E2E case.

**Bundle size** (`scripts/check_ui_bundle_size.sh`, after `make ui` and in
core-ui): warns, never fails, when the compressed UI outgrows
`ui_bundle_gzip_kb`.

Chrome 121 / Firefox 123 are the browser floor: check a change to shared styles
in both. Keep authentication in local processes; never log credentials or
authentication responses.

## Migration close-out checklist

1. When antd imports reach zero, add the `ui_raw_color` and `ui_raw_radius`
   counting rules to `scripts/arch-constraints.txt`; `check_architecture.sh`
   prints a reminder.
2. Remove the antd dependency, the React 19 compatibility patch, the temporary
   legacy interface adapters and unused `.ant-*` styles.
3. Open the dark theme entry only after every menu page passes acceptance in
   both languages, both themes, by keyboard and on the browser floor.
4. `ui/antd-inventory.json` is empty, and `scripts/ui_antd_inventory.mjs --check`
   and the browser check both pass.

## Page look

Confirmed 2026-11-06. Check every migrated page against this, by screenshot.

| Part | Rule |
| --- | --- |
| Frame | Top bar, side menu and frame share `surface-base`; the page is a white panel (radius 12, hairline `border`) 12px clear of the right and bottom edges (`--app-frame-gap`). No rule between menu and page. |
| Side menu | 200px (56 folded), folds to icons below 1280px; items 32px, 36px apart; group names `fg-subtle`; collapse button pinned at the bottom under a rule, its icon in the menu icons' column. |
| Icons | Shell icons come from `ui-kit/consoleIcons.ts` (HugeIcons, the set Zeron draws with): 16px in the menu and across the top bar, stroke 1.5. Top bar text 14px. No antd icons in the shell. |
| Page header | `PageHeader` only. Title 18/600 (Zeron `text-title`) behind the 4x18 blue bar; description 14px, one line. Breadcrumb only on pages below a menu page (`parents`), 12px above the title. Page-wide controls (`toolbar`) 12px under the title row, flush left. |
| Spacing | 16px from the header to the content and between top-level blocks; 24px between overview sections. Pages that fill the viewport subtract `--app-header-height`, `--app-frame-gap` and the panel borders. |
| Blocks | Section title 16/600 above a `Container` tray (radius 16, `rounded-2xl`); white panels inside at radius 12 (`rounded-xl`), panel title 14/600. |
| Controls | Zeron component defaults; do not restyle buttons or inputs per page. |
| Tables | `ResourceTable`: a sortable title is Zeron's `DataTableColumnHeader` (up/down mark, menu Ascending / Descending / Clear); a filterable column shows a funnel (`ui-kit/ColumnFilter`, blue while applied), the only place column filters are set; filters in effect are listed between the toolbar and the table as "column: values" chips, each clearable, plus "clear filters". The toolbar holds search and page-specific controls only. Its refresh and column-settings buttons, and the row expand chevron, are part of `ResourceTable` and use `ui-kit/consoleIcons` at 16px; pages do not add their own. Pinned header cells keep the header grey. |
| Table text | Cells 14px regular (medium-size Zeron table); a second line or a tag is 12px. Names are not bold; a name link starts flush with its title. Titles are 14/600 and never overlap: an unresized column is at least as wide as its title and marks, a narrowed one cuts the title with an ellipsis. |
| Overlays | While antd and Zeron coexist, both libraries' overlays sit on one 1000 layer (`src/theme/layers.css`): the one opened last is on top. Right-side drawers of either library open beside the AI assistant. |
| Status words | Show a state as Zeron `Badge` with `status`, not as coloured text: dark success text reads as black. |

## Choosing a component

Zeron first: a new or replaced component comes from Zeron, installed with
`scripts/zeron-registry.sh add <names>` in the change that first needs it, not
ahead of use. Compose in `ui-kit/` only what Zeron does not have, and ask the
product owner before doing so, or when a Zeron component costs bundle size or
speed (measure first).

| Need | Use |
| --- | --- |
| Page frame, header, list, detail drawer | `PageHeader`, `ResourceTable`, `DetailDrawer` (tabs, visited tabs kept) |
| Facts in a drawer ("label: value") | Zeron `detail-list` / `info-item`, replacing antd `Descriptions` |
| Overview blocks | `Container` tray with white panels (Page look, Blocks); Zeron `card` for a standalone card |
| Charts | Zeron charts (`bar-chart`, `line-chart`, `area-chart`, `pie-chart`, `heatmap-chart`, …). Check bundle size (`ui_bundle_gzip_kb`) and, for large data such as the scheduling heatmap or long series, speed against the ECharts version before switching |
| Status | Zeron `Badge` with `status` |
| Confirm, toast, empty, progress, upload, number, segmented | `ui-kit/` compositions (`ConfirmPopover`, `feedback`, `EmptyState`, `Progress`, `FileUpload`, `NumberInput`, `Segmented`) |

The registry is pinned (`ui/src/theme/zeron/source.json`); upstream changes do
not reach the console until the pin moves. Move it when a needed component or
fix exists only upstream, in its own change: new pin, reinstall, then the full
checks above (unit tests, browser check with comparison).
